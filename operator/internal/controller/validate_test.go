package controller

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	mimirv1alpha1 "github.com/SiliconSaga/mimir/operator/api/v1alpha1"
	"github.com/SiliconSaga/mimir/operator/internal/engine"
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

// The derived name must include the namespace.
//
// On a per-app cluster the object's name was unique enough because the cluster
// was too. On a shared cluster nothing else disambiguates, so falling back to
// metadata.name alone means two DataServices called "app" in different
// namespaces resolve to the same physical database — and the second to
// reconcile adopts the first tenant's data.
func TestResolvedDatabaseNameIncludesNamespace(t *testing.T) {
	d := ds(mimirv1alpha1.DataServiceSpec{Engine: mimirv1alpha1.EnginePostgres})
	got := d.ResolvedDatabaseName()
	if !strings.Contains(got, d.Namespace) {
		t.Errorf("derived name %q does not include namespace %q", got, d.Namespace)
	}

	d.Spec.DatabaseName = "custom_db"
	if got := d.ResolvedDatabaseName(); got != "custom_db" {
		t.Errorf("an explicit name must still be honoured, got %q", got)
	}
}

func TestDerivedNamesDoNotCollideAcrossNamespaces(t *testing.T) {
	a := mimirv1alpha1.DerivePhysicalName("team-a", "app")
	b := mimirv1alpha1.DerivePhysicalName("team-b", "app")
	if a == b {
		t.Fatalf("same physical name for different namespaces: %q", a)
	}

	// Truncation must not reintroduce the collision. These two share a long
	// prefix and differ only past the identifier length limit, which is exactly
	// where naive truncation would fold them together.
	long := strings.Repeat("n", 60)
	c := mimirv1alpha1.DerivePhysicalName(long+"-one", "app")
	d := mimirv1alpha1.DerivePhysicalName(long+"-two", "app")
	if c == d {
		t.Fatalf("truncation collapsed two distinct namespaces into %q", c)
	}

	// The same case but starting with a DIGIT, which takes the branch that
	// prefixes "d_". That prefix used to be followed by its own truncation, so
	// the result landed at exactly the length limit, the "too long" test in
	// DerivePhysicalName was false, and no hash was appended — collapsing these
	// two. The original test used an all-letter prefix and never came here.
	digitLong := "9" + strings.Repeat("n", 59)
	e := mimirv1alpha1.DerivePhysicalName(digitLong+"-one", "app")
	f := mimirv1alpha1.DerivePhysicalName(digitLong+"-two", "app")
	if e == f {
		t.Fatalf("digit-leading truncation collapsed two distinct namespaces into %q", e)
	}

	for _, got := range []string{a, b, c, d, e, f} {
		if err := engine.ValidateIdentifier(got); err != nil {
			t.Errorf("derived name %q is not a usable identifier: %v", got, err)
		}
		if len(got) > 63 {
			t.Errorf("derived name %q exceeds the identifier length limit at %d", got, len(got))
		}
	}
}

// Kubernetes names are DNS labels and may start with a digit or contain
// hyphens; SQL identifiers allow neither unquoted.
func TestDerivedNamesAreSQLSafe(t *testing.T) {
	for _, tc := range []struct{ ns, name string }{
		{"9team", "app"},
		{"team-a", "my-app"},
		{"team.a", "app.v2"},
	} {
		got := mimirv1alpha1.DerivePhysicalName(tc.ns, tc.name)
		if err := engine.ValidateIdentifier(got); err != nil {
			t.Errorf("DerivePhysicalName(%q, %q) = %q: %v", tc.ns, tc.name, got, err)
		}
	}
}

// A derived name that the engine would reject must fail validation as
// InvalidSpec, not surface later as a connection error pointing at the cluster.
func TestValidateChecksTheResolvedName(t *testing.T) {
	r := &DataServiceReconciler{}
	d := ds(mimirv1alpha1.DataServiceSpec{Engine: mimirv1alpha1.EnginePostgres})
	d.Namespace = "9-bad"
	d.Name = "also-bad"
	if err := r.validate(d); err != nil {
		t.Fatalf("a sanitised derived name should validate, got %v", err)
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
