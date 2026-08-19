package v1alpha1

import (
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
type DataServiceSpec struct {
	// Engine is which kind of data service to provision.
	//
	// +kubebuilder:validation:Required
	Engine Engine `json:"engine"`

	// Placement decides whether this lands in the shared cluster for its engine
	// or in a dedicated one in this namespace.
	//
	// +kubebuilder:default=shared
	// +optional
	Placement Placement `json:"placement,omitempty"`

	// DatabaseName is the database to create. Defaults to metadata.name.
	//
	// Must be a valid unquoted PostgreSQL identifier, which is stricter than a
	// Kubernetes name: no leading digit, and underscores rather than hyphens.
	// Enforced here so a bad name fails at admission rather than deep inside a
	// reconcile as a syntax error.
	//
	// +kubebuilder:validation:Pattern=`^[a-z_][a-z0-9_]*$`
	// +kubebuilder:validation:MaxLength=63
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

// ResolvedDatabaseName returns the database name to use, defaulting to the
// object's own name when the field is unset.
func (d *DataService) ResolvedDatabaseName() string {
	if d.Spec.DatabaseName != "" {
		return d.Spec.DatabaseName
	}
	return d.Name
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
