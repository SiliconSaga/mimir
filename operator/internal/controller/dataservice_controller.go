package controller

import (
	"context"
	"fmt"
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
	// AdminDatabase to connect to for administering others.
	AdminDatabase string
	// TLS is whether the server requires an encrypted connection.
	TLS bool
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

// +kubebuilder:rbac:groups=mimir.siliconsaga.org,resources=dataservices,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=mimir.siliconsaga.org,resources=dataservices/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=mimir.siliconsaga.org,resources=dataservices/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch

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
	existing := r.existingPassword(ctx, ds.Namespace, secretName)

	creds, err := prov.Ensure(ctx, target, dbName, existing, engine.Options{Extensions: ds.Spec.Extensions})
	if err != nil {
		l.Error(err, "provisioning failed", "database", dbName)
		r.setReady(&ds, metav1.ConditionFalse, mimirv1alpha1.ReasonConnectionFailed, err.Error())
		ds.Status.Phase = "Failed"
		if statusErr := r.patchStatus(ctx, &ds); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{RequeueAfter: requeueAfterTransient}, nil
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
	return nil
}

func (r *DataServiceReconciler) reconcileDelete(ctx context.Context, ds *mimirv1alpha1.DataService) (ctrl.Result, error) {
	l := log.FromContext(ctx)
	if !controllerutil.ContainsFinalizer(ds, finalizer) {
		return ctrl.Result{}, nil
	}

	prov, ok := r.Registry.For(ds.Spec.Engine)
	shared, hasCluster := r.SharedClusters[ds.Spec.Engine]

	if ok && hasCluster && ds.ResolvedPlacement() == mimirv1alpha1.PlacementShared {
		target, err := r.resolveTarget(ctx, shared)
		if err != nil {
			// Do not block deletion forever on an unreachable cluster: that
			// strands the object with a finalizer nobody can clear by hand.
			// The database is left behind and logged loudly instead.
			l.Error(err, "cannot reach cluster to drop database; releasing finalizer anyway",
				"database", ds.ResolvedDatabaseName())
		} else if err := prov.Drop(ctx, target, ds.ResolvedDatabaseName()); err != nil {
			l.Error(err, "dropping database failed; releasing finalizer anyway",
				"database", ds.ResolvedDatabaseName())
		}
	}

	controllerutil.RemoveFinalizer(ds, finalizer)
	return ctrl.Result{}, r.Update(ctx, ds)
}

// resolveTarget reads the admin credentials for a shared cluster.
func (r *DataServiceReconciler) resolveTarget(ctx context.Context, s SharedCluster) (engine.Target, error) {
	var secret corev1.Secret
	key := types.NamespacedName{Name: s.AdminSecretName, Namespace: s.AdminSecretNamespace}
	if err := r.Get(ctx, key, &secret); err != nil {
		return engine.Target{}, fmt.Errorf("read admin secret %s: %w", key, err)
	}

	user, ok := secret.Data[s.AdminUserKey]
	if !ok {
		return engine.Target{}, fmt.Errorf("admin secret %s has no key %q", key, s.AdminUserKey)
	}
	pass, ok := secret.Data[s.AdminPasswordKey]
	if !ok {
		return engine.Target{}, fmt.Errorf("admin secret %s has no key %q", key, s.AdminPasswordKey)
	}

	return engine.Target{
		Host:          s.Host,
		Port:          s.Port,
		AdminHost:     s.AdminHost,
		AdminPort:     s.AdminPort,
		AdminUser:     string(user),
		AdminPassword: string(pass),
		AdminDatabase: s.AdminDatabase,
		TLS:           s.TLS,
	}, nil
}

// existingPassword returns the password already published, or "" if there is
// no Secret yet.
func (r *DataServiceReconciler) existingPassword(ctx context.Context, ns, name string) string {
	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &secret); err != nil {
		return ""
	}
	return string(secret.Data["password"])
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
		secret.StringData = map[string]string{
			"host":     c.Host,
			"port":     fmt.Sprintf("%d", c.Port),
			"database": c.Database,
			"username": c.Username,
			"password": c.Password,
			"uri":      c.URI,
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
	if err := r.Status().Update(ctx, ds); err != nil {
		if apierrors.IsConflict(err) {
			// A conflict means someone else wrote first; the next reconcile
			// recomputes from fresh state rather than clobbering theirs.
			return nil
		}
		return err
	}
	return nil
}

// SetupWithManager wires the controller up.
func (r *DataServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&mimirv1alpha1.DataService{}).
		Owns(&corev1.Secret{}).
		Complete(r)
}
