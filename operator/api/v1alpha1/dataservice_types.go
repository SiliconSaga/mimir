package v1alpha1

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Engine is the kind of data service being requested.
//
// Deliberately only the three databases that share a model — a named database
// with an owning identity and a credential. Kafka and Valkey are excluded on
// purpose: Kafka vends topics (plural) plus a separate identity carrying ACLs
// and quotas, and the Valkey operator models no per-tenant object at all.
// Forcing either through databaseName would lose expressiveness for no gain,
// and Strimzi's KafkaTopic/KafkaUser already do the job properly.
//
// +kubebuilder:validation:Enum=postgres;mysql;mongodb
type Engine string

const (
	EnginePostgres Engine = "postgres"
	EngineMySQL    Engine = "mysql"
	EngineMongoDB  Engine = "mongodb"
)

// Placement selects a database on the shared cluster, or a cluster of its own.
//
// +kubebuilder:validation:Enum=shared;dedicated
type Placement string

const (
	// PlacementShared vends a database inside the platform's shared cluster.
	PlacementShared Placement = "shared"
	// PlacementDedicated provisions a cluster in the app's own namespace —
	// the behaviour every claim had before this API existed.
	PlacementDedicated Placement = "dedicated"
)

// DataServiceSpec describes a requested data service.
//
// Engine, placement and databaseName are IMMUTABLE after creation, enforced at
// admission. Together they name a database that already exists and holds data,
// so editing one does not move anything — it re-points the object at a
// different database and silently abandons the first. Changing databaseName
// would provision a second database and orphan the original; changing placement
// would make cleanup skip the shared database entirely. Rejecting the edit is
// the honest answer: delete the DataService and declare a new one.
type DataServiceSpec struct {
	// Engine is which kind of data service to provision.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="engine is immutable once provisioned — delete and recreate the DataService instead"
	Engine Engine `json:"engine"`

	// Placement decides whether this lands in the shared cluster for its engine
	// or in a dedicated one in this namespace.
	//
	// +kubebuilder:default=shared
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="placement is immutable once provisioned — delete and recreate the DataService instead"
	// +optional
	Placement Placement `json:"placement,omitempty"`

	// DatabaseName is the database to create. When unset, it is derived from
	// the namespace AND the object name — not from metadata.name alone, since
	// on a shared cluster nothing else keeps two identically-named requests in
	// different namespaces apart.
	//
	// Must be a valid unquoted PostgreSQL identifier, which is stricter than a
	// Kubernetes name: no leading digit, and underscores rather than hyphens.
	// Enforced here so a bad name fails at admission rather than deep inside a
	// reconcile as a syntax error.
	//
	// +kubebuilder:validation:Pattern=`^[a-z_][a-z0-9_]*$`
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="databaseName is immutable once provisioned — delete and recreate the DataService instead"
	// +optional
	DatabaseName string `json:"databaseName,omitempty"`

	// Version pins the engine's major version.
	//
	// Only legal with placement: dedicated. A shared cluster runs one version
	// for every tenant, so accepting this there would be a promise the API
	// cannot keep — and silently ignoring it is the dead-config failure mode
	// that looks correct for months. An app that genuinely needs a different
	// version is telling us it needs a dedicated cluster.
	//
	// +optional
	Version string `json:"version,omitempty"`

	// Extensions to enable in the database. PostgreSQL only.
	//
	// +optional
	Extensions []string `json:"extensions,omitempty"`
}

// DataServiceStatus reports what was provisioned.
type DataServiceStatus struct {
	// Phase is a coarse human-facing summary. Conditions carry the detail.
	//
	// +optional
	Phase string `json:"phase,omitempty"`

	// SecretName is the Secret in this namespace holding the connection
	// details. Reported here so consumers never have to guess the name —
	// the same contract Strimzi's KafkaUser uses.
	//
	// +optional
	SecretName string `json:"secretName,omitempty"`

	// Host and Port are where the database actually answers.
	//
	// +optional
	Host string `json:"host,omitempty"`
	// +optional
	Port int32 `json:"port,omitempty"`

	// ProvisionedDatabase is the physical database name that was actually
	// created, recorded at provisioning time.
	//
	// Deletion reads THIS rather than re-deriving from the spec. Re-derivation
	// cleans up whatever the spec says today, which is not necessarily what
	// exists: an edited databaseName would leave the original database behind
	// and drop a name that may belong to someone else. The spec fields are
	// immutable, so the two cannot normally diverge — this is the belt to that
	// braces, and it also covers objects created before the rule existed.
	// +optional
	ProvisionedDatabase string `json:"provisionedDatabase,omitempty"`

	// ObservedGeneration is the spec generation this status reflects.
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions follow the standard Kubernetes convention. Ready means the
	// database, its role and its Secret all exist and agree.
	//
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

const (
	// ConditionReady is true once the database, role and Secret all exist.
	ConditionReady = "Ready"

	ReasonProvisioned      = "Provisioned"
	ReasonProvisioning     = "Provisioning"
	ReasonInvalidSpec      = "InvalidSpec"
	ReasonClusterNotFound  = "ClusterNotFound"
	ReasonConnectionFailed = "ConnectionFailed"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=dsvc
// +kubebuilder:printcolumn:name="Engine",type=string,JSONPath=`.spec.engine`
// +kubebuilder:printcolumn:name="Placement",type=string,JSONPath=`.spec.placement`
// +kubebuilder:printcolumn:name="Database",type=string,JSONPath=`.spec.databaseName`
// +kubebuilder:printcolumn:name="Secret",type=string,JSONPath=`.status.secretName`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// DataService is a request for a database inside an existing server.
type DataService struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DataServiceSpec   `json:"spec,omitempty"`
	Status DataServiceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// DataServiceList contains a list of DataService.
type DataServiceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DataService `json:"items"`
}

func init() {
	SchemeBuilder.Register(&DataService{}, &DataServiceList{})
}

// maxIdentifier is PostgreSQL's NAMEDATALEN-1. MySQL allows 64, so the lower
// of the two is used everywhere rather than per-engine.
const maxIdentifier = 63

// ResolvedDatabaseName returns the physical database name on the server.
//
// When spec.databaseName is unset this derives a name from namespace AND
// object name, not from the object name alone. On a per-app cluster the
// object's name was unique enough because the cluster was too — on a SHARED
// cluster nothing else disambiguates, so two DataServices called "app" in
// different namespaces would otherwise resolve to the same physical database
// and the second to reconcile would adopt the first tenant's data.
//
// An explicit spec.databaseName is still honoured, because an app may need a
// particular name and the value is already constrained by the CRD pattern.
// That path is protected at provisioning time instead: the engine records an
// ownership marker on the database and refuses to touch one it does not own.
func (d *DataService) ResolvedDatabaseName() string {
	if d.Spec.DatabaseName != "" {
		return d.Spec.DatabaseName
	}
	return DerivePhysicalName(d.Namespace, d.Name)
}

// Owner is the identity recorded on the provisioned database so a later
// reconcile — of this object or any other — can tell whose it is.
func (d *DataService) Owner() string {
	return d.Namespace + "/" + d.Name
}

// DerivePhysicalName builds a valid, collision-free SQL identifier from a
// namespace and name.
//
// Kubernetes names are DNS labels: lower-case, hyphen-separated, possibly
// starting with a digit. SQL identifiers allow none of that unquoted, so
// hyphens and dots become underscores and a leading digit gets a prefix.
//
// Truncation keeps a hash of the FULL input, so two long namespaces sharing a
// prefix stay distinct — plain truncation would reintroduce exactly the
// collision this function exists to prevent.
//
// Length reduction happens HERE and nowhere else. An earlier version also
// truncated inside sanitizeIdentifier after prefixing a leading digit, which
// produced a string of exactly maxIdentifier — so the `>` test below was false,
// no hash was appended, and two digit-leading namespaces differing only past
// character 61 collided. Two places that shorten a string is one too many.
func DerivePhysicalName(namespace, name string) string {
	full := namespace + "_" + name
	sanitized := sanitizeIdentifier(full)

	if len(sanitized) > maxIdentifier {
		sum := sha256.Sum256([]byte(full))
		suffix := "_" + hex.EncodeToString(sum[:])[:8]
		sanitized = sanitized[:maxIdentifier-len(suffix)] + suffix
	}
	return sanitized
}

// sanitizeIdentifier maps a Kubernetes name onto the SQL identifier alphabet.
// It never shortens: length is DerivePhysicalName's job alone.
//
// Note this is lossy, and deliberately so. Kubernetes namespaces are DNS-1123
// labels (lower-case, digits, '-'), and object names may also carry '.', all of
// which fold to '_'. So "app.v2" and "app-v2" in one namespace derive the same
// physical name. That is a real collision, but a SAFE one: the ownership marker
// refuses the second request with a Conflict rather than letting it adopt the
// first tenant's database. Visible and stuck beats silent and shared.
func sanitizeIdentifier(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := b.String()
	if out == "" {
		return "db"
	}
	// An identifier may not begin with a digit unquoted.
	if out[0] >= '0' && out[0] <= '9' {
		out = "d_" + out
	}
	return out
}

// ResolvedPlacement defaults to shared when unset, matching the CRD default.
// Repeated here so the controller behaves the same on an object created
// before the default existed.
func (d *DataService) ResolvedPlacement() Placement {
	if d.Spec.Placement != "" {
		return d.Spec.Placement
	}
	return PlacementShared
}
