package controller

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	mimirv1alpha1 "github.com/SiliconSaga/mimir/operator/api/v1alpha1"
)

func ds(spec mimirv1alpha1.DataServiceSpec) *mimirv1alpha1.DataService {
	return &mimirv1alpha1.DataService{
		ObjectMeta: metav1.ObjectMeta{Name: "forgejo", Namespace: "forgejo"},
		Spec:       spec,
	}
}

// spec.version under placement: shared is the rule the OpenAPI schema cannot
// express, so it only exists here. If this test goes away the field starts
// being silently ignored, which is the dead-config failure the design calls out.
func TestVersionRejectedOnShared(t *testing.T) {
	r := &DataServiceReconciler{}

	err := r.validate(ds(mimirv1alpha1.DataServiceSpec{
		Engine:    mimirv1alpha1.EnginePostgres,
		Placement: mimirv1alpha1.PlacementShared,
		Version:   "16",
	}))
	if err == nil {
		t.Fatal("expected version+shared to be rejected")
	}
	if !strings.Contains(err.Error(), "dedicated") {
		t.Errorf("error should point at the way out, got: %v", err)
	}
}

// Unset placement defaults to shared, so the rule must apply there too —
// otherwise omitting the field becomes a way around it.
func TestVersionRejectedWhenPlacementUnset(t *testing.T) {
	r := &DataServiceReconciler{}
	err := r.validate(ds(mimirv1alpha1.DataServiceSpec{
		Engine:  mimirv1alpha1.EnginePostgres,
		Version: "16",
	}))
	if err == nil {
		t.Fatal("expected version to be rejected when placement is unset (defaults to shared)")
	}
}

func TestVersionAllowedOnDedicated(t *testing.T) {
	r := &DataServiceReconciler{}
	err := r.validate(ds(mimirv1alpha1.DataServiceSpec{
		Engine:    mimirv1alpha1.EnginePostgres,
		Placement: mimirv1alpha1.PlacementDedicated,
		Version:   "16",
	}))
	if err != nil {
		t.Fatalf("version should be allowed with dedicated placement, got %v", err)
	}
}

func TestExtensionsArePostgresOnly(t *testing.T) {
	r := &DataServiceReconciler{}

	if err := r.validate(ds(mimirv1alpha1.DataServiceSpec{
		Engine:     mimirv1alpha1.EngineMySQL,
		Extensions: []string{"pgcrypto"},
	})); err == nil {
		t.Fatal("expected extensions on mysql to be rejected")
	}

	if err := r.validate(ds(mimirv1alpha1.DataServiceSpec{
		Engine:     mimirv1alpha1.EnginePostgres,
		Extensions: []string{"pgcrypto"},
	})); err != nil {
		t.Fatalf("extensions on postgres should be allowed, got %v", err)
	}
}

func TestResolvedDatabaseNameFallsBackToObjectName(t *testing.T) {
	d := ds(mimirv1alpha1.DataServiceSpec{Engine: mimirv1alpha1.EnginePostgres})
	if got := d.ResolvedDatabaseName(); got != "forgejo" {
		t.Errorf("expected fallback to metadata.name, got %q", got)
	}

	d.Spec.DatabaseName = "custom_db"
	if got := d.ResolvedDatabaseName(); got != "custom_db" {
		t.Errorf("expected the explicit name, got %q", got)
	}
}

// The controller must default the same way the CRD does, so an object created
// before the default existed behaves identically.
func TestResolvedPlacementDefaultsToShared(t *testing.T) {
	d := ds(mimirv1alpha1.DataServiceSpec{Engine: mimirv1alpha1.EnginePostgres})
	if got := d.ResolvedPlacement(); got != mimirv1alpha1.PlacementShared {
		t.Errorf("expected shared by default, got %q", got)
	}
}

// setReady must not restamp LastTransitionTime when the status is unchanged,
// or "last transition" silently becomes "last reconcile" and the field stops
// being able to answer how long something has been broken.
func TestSetReadyPreservesTransitionTime(t *testing.T) {
	r := &DataServiceReconciler{}
	d := ds(mimirv1alpha1.DataServiceSpec{Engine: mimirv1alpha1.EnginePostgres})

	r.setReady(d, metav1.ConditionTrue, mimirv1alpha1.ReasonProvisioned, "first")
	first := d.Status.Conditions[0].LastTransitionTime

	r.setReady(d, metav1.ConditionTrue, mimirv1alpha1.ReasonProvisioned, "second")
	if !d.Status.Conditions[0].LastTransitionTime.Equal(&first) {
		t.Error("LastTransitionTime changed while status stayed True")
	}
	if d.Status.Conditions[0].Message != "second" {
		t.Error("message should still update even when status is unchanged")
	}

	r.setReady(d, metav1.ConditionFalse, mimirv1alpha1.ReasonConnectionFailed, "down")
	if len(d.Status.Conditions) != 1 {
		t.Fatalf("expected the condition to be replaced, got %d", len(d.Status.Conditions))
	}
	if d.Status.Conditions[0].Status != metav1.ConditionFalse {
		t.Error("status should have flipped to False")
	}
}
