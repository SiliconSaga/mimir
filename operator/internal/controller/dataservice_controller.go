package controller

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	mimirv1alpha1 "github.com/SiliconSaga/mimir/operator/api/v1alpha1"
	"github.com/SiliconSaga/mimir/operator/internal/engine"
)

const (
	// finalizer keeps the object around long enough to drop the database it
	// created. Without it, deleting the DataService orphans a database and a
	// role on the shared cluster with nothing left pointing at them.
	finalizer = "mimir.siliconsaga.org/dataservice"

	requeueAfterTransient = 30 * time.Second
	requeueSteadyState    = 10 * time.Minute

	// forceDeleteAnnotation lets an operator delete a DataService whose
	// database cannot be dropped — an unreachable cluster, or one already gone.
	// Deliberately annotation-driven rather than automatic: orphaning tenant
	// data on a shared cluster should be a decision someone made.
	forceDeleteAnnotation = "mimir.siliconsaga.org/force-delete"
)

// SharedCluster describes where an engine's shared server lives and how to
// administer it. Supplied by configuration rather than discovered, so the
// operator has no opinion about how the cluster itself is deployed.
type SharedCluster struct {
	// Host and Port consumers connect to — the pooler where one exists.
	Host string
	Port int32
	// AdminHost and AdminPort are where DDL runs: the primary. A pooler in
	// transaction mode cannot carry CREATE DATABASE.
	AdminHost string
	AdminPort int32
	// AdminSecret is the Secret holding admin credentials, and the keys within it.
	AdminSecretName      string
	AdminSecretNamespace string
	AdminUserKey         string
	AdminPasswordKey     string
	// AdminUser is the admin account name given literally, used INSTEAD of
	// reading AdminUserKey from the Secret.
	//
	// Needed because operators disagree about Secret shape. Percona's Postgres
	// operator publishes <cluster>-pguser-<user> containing both `user` and
	// `password`, so a key lookup works. Percona's XtraDB operator publishes
	// <cluster>-secrets whose KEYS ARE THE USERNAMES and whose values are the
	// passwords — `root`, `monitor`, `xtrabackup`. There is no key holding the
	// string "root", so no value of AdminUserKey can resolve it, and a
	// key-only design simply cannot be pointed at a PXC cluster.
	AdminUser string
	// AdminDatabase to connect to for administering others.
	AdminDatabase string
	// TLS is whether the server requires an encrypted connection.
	TLS bool
	// PoolerAuthRole is the role the pooler authenticates as when it looks a
	// client's password up. Vended databases have to admit it explicitly or the
	// published URI cannot connect at all.
	PoolerAuthRole string
}

// DataServiceReconciler reconciles a DataService object.
type DataServiceReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Registry maps an engine to its provisioner.
	Registry *engine.Registry

	// SharedClusters is the shared server per engine. An engine absent here has
	// no shared cluster configured, which is reportable rather than fatal.
	SharedClusters map[mimirv1alpha1.Engine]SharedCluster
}

// The controller watches and updates DataService objects but never creates or
// deletes them — those come from users and from garbage collection — so it
// does not ask for create or delete.
//
// Secrets are get/create/update/patch only. It reads one admin Secret by name
// and writes one published Secret per tenant, so it never needs to enumerate
// Secrets — and list/watch cluster-wide would let a compromised operator read
// every credential in the cluster rather than the ones it manages.
// +kubebuilder:rbac:groups=mimir.siliconsaga.org,resources=dataservices,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=mimir.siliconsaga.org,resources=dataservices/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=mimir.siliconsaga.org,resources=dataservices/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;create;update;patch

func (r *DataServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)

	var ds mimirv1alpha1.DataService
	if err := r.Get(ctx, req.NamespacedName, &ds); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !ds.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &ds)
	}

	// Validate before touching anything. A rejected spec is a terminal state,
	// not something to retry — requeueing an invalid spec just burns the API
	// server until a human edits it.
	if err := r.validate(&ds); err != nil {
		l.Info("spec rejected", "reason", err.Error())
		r.setReady(&ds, metav1.ConditionFalse, mimirv1alpha1.ReasonInvalidSpec, err.Error())
		ds.Status.Phase = "Invalid"
		return ctrl.Result{}, r.patchStatus(ctx, &ds)
	}

	if ds.ResolvedPlacement() == mimirv1alpha1.PlacementDedicated {
		// Dedicated is the pre-existing cluster-per-app behaviour, still vended
		// by the Crossplane claims. Not reimplemented here; the API accepts the
		// value so an app's manifest does not change shape when it moves.
		r.setReady(&ds, metav1.ConditionFalse, mimirv1alpha1.ReasonInvalidSpec,
			"placement: dedicated is not implemented by this operator yet — use the PostgreSQLInstance claim")
		ds.Status.Phase = "Unsupported"
		return ctrl.Result{}, r.patchStatus(ctx, &ds)
	}

	prov, ok := r.Registry.For(ds.Spec.Engine)
	if !ok {
		r.setReady(&ds, metav1.ConditionFalse, mimirv1alpha1.ReasonInvalidSpec,
			fmt.Sprintf("engine %q has no provisioner in this build", ds.Spec.Engine))
		ds.Status.Phase = "Unsupported"
		return ctrl.Result{}, r.patchStatus(ctx, &ds)
	}

	shared, ok := r.SharedClusters[ds.Spec.Engine]
	if !ok {
		r.setReady(&ds, metav1.ConditionFalse, mimirv1alpha1.ReasonClusterNotFound,
			fmt.Sprintf("no shared cluster configured for engine %q", ds.Spec.Engine))
		ds.Status.Phase = "Pending"
		return ctrl.Result{RequeueAfter: requeueAfterTransient}, r.patchStatus(ctx, &ds)
	}

	if !controllerutil.ContainsFinalizer(&ds, finalizer) {
		controllerutil.AddFinalizer(&ds, finalizer)
		if err := r.Update(ctx, &ds); err != nil {
			return ctrl.Result{}, err
		}
	}

	target, err := r.resolveTarget(ctx, shared)
	if err != nil {
		r.setReady(&ds, metav1.ConditionFalse, mimirv1alpha1.ReasonClusterNotFound, err.Error())
		ds.Status.Phase = "Pending"
		return ctrl.Result{RequeueAfter: requeueAfterTransient}, r.patchStatus(ctx, &ds)
	}

	dbName := ds.ResolvedDatabaseName()
	secretName := ds.Name + "-dataservice"

	// Reuse the existing password if a Secret is already present. Generating a
	// fresh one each reconcile would rotate the credential out from under every
	// consumer roughly every ten minutes.
	existing, err := r.existingPassword(ctx, ds.Namespace, secretName)
	if err != nil {
		r.setReady(&ds, metav1.ConditionFalse, mimirv1alpha1.ReasonConnectionFailed, err.Error())
		ds.Status.Phase = "Pending"
		if statusErr := r.patchStatus(ctx, &ds); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{RequeueAfter: requeueAfterTransient}, nil
	}

	creds, err := prov.Ensure(ctx, target, dbName, existing, engine.Options{
		Extensions: ds.Spec.Extensions,
		Owner:      ds.Owner(),
	})
	if err != nil {
		// A conflict is not transient and will not clear by retrying — another
		// DataService already owns this physical database. Report it as such
		// and stop hammering the server.
		var notOwned *engine.ErrNotOwned
		if errors.As(err, &notOwned) {
			l.Error(err, "refusing to adopt a database owned by another DataService",
				"database", dbName)
			r.setReady(&ds, metav1.ConditionFalse, mimirv1alpha1.ReasonInvalidSpec, err.Error())
			ds.Status.Phase = "Conflict"
			return ctrl.Result{}, r.patchStatus(ctx, &ds)
		}
		l.Error(err, "provisioning failed", "database", dbName)
		r.setReady(&ds, metav1.ConditionFalse, mimirv1alpha1.ReasonConnectionFailed, err.Error())
		ds.Status.Phase = "Failed"
		if statusErr := r.patchStatus(ctx, &ds); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{RequeueAfter: requeueAfterTransient}, nil
	}

	// Persist the physical name IMMEDIATELY, before anything else can fail.
	//
	// The database and role now exist. dropShared refuses to guess a name and
	// returns early when status.provisionedDatabase is empty, so if this were
	// recorded only in the final patch below, a failure in writeSecret — or in
	// that patch itself — would leave a provisioned database with no record of
	// it. Deleting the object then releases the finalizer and orphans both,
	// and the leftover name blocks the next request for it with an ownership
	// conflict. Exactly the orphaning the deletion path exists to prevent.
	if ds.Status.ProvisionedDatabase != dbName {
		ds.Status.ProvisionedDatabase = dbName
		if err := r.patchStatus(ctx, &ds); err != nil {
			return ctrl.Result{}, err
		}
	}

	if err := r.writeSecret(ctx, &ds, secretName, creds); err != nil {
		return ctrl.Result{}, err
	}

	ds.Status.Phase = "Ready"
	ds.Status.SecretName = secretName
	ds.Status.Host = creds.Host
	ds.Status.Port = creds.Port
	r.setReady(&ds, metav1.ConditionTrue, mimirv1alpha1.ReasonProvisioned,
		fmt.Sprintf("database %q is available", dbName))

	if err := r.patchStatus(ctx, &ds); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeueSteadyState}, nil
}

// validate enforces the rules the CRD schema cannot express on its own.
func (r *DataServiceReconciler) validate(ds *mimirv1alpha1.DataService) error {
	// A shared cluster runs one version for every tenant, so honouring a
	// per-request version there is impossible. Rejecting is the honest answer;
	// ignoring it silently is the failure mode that looks fine for months.
	if ds.Spec.Version != "" && ds.ResolvedPlacement() == mimirv1alpha1.PlacementShared {
		return fmt.Errorf(
			"spec.version is not allowed with placement: shared — the platform serves one version per engine; use placement: dedicated to pin your own")
	}
	if len(ds.Spec.Extensions) > 0 && ds.Spec.Engine != mimirv1alpha1.EnginePostgres {
		return fmt.Errorf("spec.extensions is only valid for engine: postgres")
	}
	// Checked here, before the finalizer and before Ensure. Validating only at
	// the point of use meant a typo was found after the role, database, marker,
	// password and grants had all been written — leaving partial state, and
	// retrying every thirty seconds on a spec that can never succeed. It is a
	// terminal InvalidSpec, so it should be terminal from the start.
	if err := engine.ValidateExtensions(ds.Spec.Extensions); err != nil {
		return err
	}
	// Validate the name that will actually be used, not just the field. The
	// CRD pattern constrains spec.databaseName, but when it is unset the name
	// is derived — and a derived name that the engine later rejects would
	// surface as ConnectionFailed, which points at the wrong thing entirely.
	if err := engine.ValidateIdentifier(ds.ResolvedDatabaseName()); err != nil {
		return fmt.Errorf("resolved database name %q is not usable: %w", ds.ResolvedDatabaseName(), err)
	}
	return nil
}

func (r *DataServiceReconciler) reconcileDelete(ctx context.Context, ds *mimirv1alpha1.DataService) (ctrl.Result, error) {
	l := log.FromContext(ctx)
	if !controllerutil.ContainsFinalizer(ds, finalizer) {
		return ctrl.Result{}, nil
	}

	// The escape hatch is explicit and human-set. Releasing automatically on
	// failure — as an earlier version did — silently orphans a tenant's data on
	// the shared cluster, where it then blocks the next request for that name
	// with a conflict nobody can explain.
	force := ds.Annotations[forceDeleteAnnotation] == "true"

	// Only shared placement has a database here to drop. `dedicated` still runs
	// through the old PostgreSQLInstance claim, which owns its own lifecycle,
	// so there is genuinely nothing for this controller to clean up.
	if ds.ResolvedPlacement() == mimirv1alpha1.PlacementShared {
		if err := r.dropShared(ctx, ds); err != nil {
			if !force {
				l.Error(err, "cannot drop database; keeping finalizer so the data is not orphaned",
					"database", ds.Status.ProvisionedDatabase,
					"override", forceDeleteAnnotation+"=true")
				return ctrl.Result{RequeueAfter: requeueAfterTransient}, nil
			}
			l.Error(err, "dropping database failed; releasing finalizer because the override is set",
				"database", ds.ResolvedDatabaseName())
		}
	}

	controllerutil.RemoveFinalizer(ds, finalizer)
	return ctrl.Result{}, r.Update(ctx, ds)
}

// dropShared removes the database backing a shared-placement DataService.
//
// A missing provisioner or missing shared cluster is treated as a FAILURE to
// clean up, not as "nothing to do". Both are read from process configuration —
// the registry from the build, the cluster map from environment — so a
// redeploy with MIMIR_POSTGRES_HOST unset makes the map empty. Skipping the
// drop in that window would release the finalizer and leave the database and
// role behind on the shared cluster, which is precisely the orphaning the
// caller's comment says it prevents, and the leftover name then blocks the
// next request for it with an ownership conflict.
func (r *DataServiceReconciler) dropShared(ctx context.Context, ds *mimirv1alpha1.DataService) error {
	// Nothing was ever provisioned — a request that failed validation, or lost
	// an ownership conflict before Ensure returned. There is no database of
	// ours to drop, and guessing a name here is how the conflict-losing object
	// would delete the winner's database.
	db := ds.Status.ProvisionedDatabase
	if db == "" {
		return nil
	}

	prov, ok := r.Registry.For(ds.Spec.Engine)
	if !ok {
		return fmt.Errorf("no provisioner for engine %q in this build, so %q cannot be dropped", ds.Spec.Engine, db)
	}
	shared, hasCluster := r.SharedClusters[ds.Spec.Engine]
	if !hasCluster {
		return fmt.Errorf("no shared cluster configured for engine %q, so %q cannot be dropped", ds.Spec.Engine, db)
	}

	target, err := r.resolveTarget(ctx, shared)
	if err != nil {
		return err
	}
	// The owner is passed so the provisioner refuses to drop a database that
	// is not ours, even if the name matches.
	return prov.Drop(ctx, target, db, ds.Owner())
}

// resolveTarget reads the admin credentials for a shared cluster.
func (r *DataServiceReconciler) resolveTarget(ctx context.Context, s SharedCluster) (engine.Target, error) {
	var secret corev1.Secret
	key := types.NamespacedName{Name: s.AdminSecretName, Namespace: s.AdminSecretNamespace}
	if err := r.Get(ctx, key, &secret); err != nil {
		return engine.Target{}, fmt.Errorf("read admin secret %s: %w", key, err)
	}

	// A literal admin user wins over a key lookup. The password still comes
	// from the Secret either way — that is the part that must not be config.
	adminUser := s.AdminUser
	if adminUser == "" {
		user, ok := secret.Data[s.AdminUserKey]
		if !ok {
			return engine.Target{}, fmt.Errorf("admin secret %s has no key %q "+
				"(set the engine's ADMIN_USER if the operator publishes passwords under username keys)",
				key, s.AdminUserKey)
		}
		adminUser = string(user)
	}

	pass, ok := secret.Data[s.AdminPasswordKey]
	if !ok {
		return engine.Target{}, fmt.Errorf("admin secret %s has no key %q", key, s.AdminPasswordKey)
	}

	return engine.Target{
		Host:           s.Host,
		Port:           s.Port,
		AdminHost:      s.AdminHost,
		AdminPort:      s.AdminPort,
		AdminUser:      adminUser,
		AdminPassword:  string(pass),
		AdminDatabase:  s.AdminDatabase,
		TLS:            s.TLS,
		PoolerAuthRole: s.PoolerAuthRole,
	}, nil
}

// existingPassword returns the password already published.
//
// Only a genuine NotFound yields "" — every other read failure is propagated.
// Treating a transient API error as "no password yet" would make the next
// Ensure generate a fresh one and rotate the credential out from under a
// working consumer, turning a momentary blip into a broken service.
func (r *DataServiceReconciler) existingPassword(ctx context.Context, ns, name string) (string, error) {
	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("read published secret %s/%s: %w", ns, name, err)
	}
	pw, ok := secret.Data["password"]
	if !ok {
		// The Secret exists but is malformed. Generating a replacement would
		// silently rotate; refusing surfaces it while the database still works.
		return "", fmt.Errorf("published secret %s/%s has no password key", ns, name)
	}
	return string(pw), nil
}

func (r *DataServiceReconciler) writeSecret(ctx context.Context, ds *mimirv1alpha1.DataService, name string, c engine.Credentials) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ds.Namespace},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		if secret.Labels == nil {
			secret.Labels = map[string]string{}
		}
		secret.Labels["app.kubernetes.io/managed-by"] = "mimir-dataservice"
		secret.Labels["mimir.siliconsaga.org/dataservice"] = ds.Name

		secret.Type = corev1.SecretTypeOpaque
		// Data, not StringData. StringData is write-only — a GET never returns
		// it — so CreateOrUpdate compares an always-empty field against a
		// populated one and issues an update on every single reconcile, even
		// when nothing changed. Writing Data makes the comparison stable and
		// the reconcile a genuine no-op in the common case.
		secret.Data = map[string][]byte{
			"host":     []byte(c.Host),
			"port":     []byte(strconv.Itoa(int(c.Port))),
			"database": []byte(c.Database),
			"username": []byte(c.Username),
			"password": []byte(c.Password),
			"uri":      []byte(c.URI),
		}
		// Owner reference so the Secret is garbage-collected with the request.
		return controllerutil.SetControllerReference(ds, secret, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("write secret %s/%s: %w", ds.Namespace, name, err)
	}
	return nil
}

func (r *DataServiceReconciler) setReady(ds *mimirv1alpha1.DataService, status metav1.ConditionStatus, reason, msg string) {
	meta := metav1.Condition{
		Type:               mimirv1alpha1.ConditionReady,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: ds.Generation,
		LastTransitionTime: metav1.Now(),
	}
	for i, c := range ds.Status.Conditions {
		if c.Type == meta.Type {
			// Preserve the original transition time when nothing actually
			// changed, or "last transition" becomes "last reconcile".
			if c.Status == meta.Status {
				meta.LastTransitionTime = c.LastTransitionTime
			}
			ds.Status.Conditions[i] = meta
			return
		}
	}
	ds.Status.Conditions = append(ds.Status.Conditions, meta)
}

func (r *DataServiceReconciler) patchStatus(ctx context.Context, ds *mimirv1alpha1.DataService) error {
	ds.Status.ObservedGeneration = ds.Generation
	// Conflicts are returned, not swallowed. Dropping one loses the status
	// update with no requeue, so a Ready object can sit reporting a stale
	// phase until the next steady-state pass ten minutes later.
	// controller-runtime backs off and retries with fresh state.
	return r.Status().Update(ctx, ds)
}

// SetupWithManager wires the controller up.
//
// Deliberately NOT Owns(&corev1.Secret{}). That builds a Secret informer, which
// LISTs and WATCHes the type cluster-wide — permissions this operator no longer
// holds, and deliberately so. The manager would then block forever waiting for
// a cache it cannot fill, which presents as an operator that starts and does
// nothing rather than as a permissions error.
//
// The cost is latency, not correctness: a published Secret deleted by hand is
// restored on the next reconcile rather than immediately, and the
// OwnerReference still garbage-collects it with the DataService. Watching every
// Secret in the cluster to notice edits to our own is a poor trade.
func (r *DataServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&mimirv1alpha1.DataService{}).
		Complete(r)
}
