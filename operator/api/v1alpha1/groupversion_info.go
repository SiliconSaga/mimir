// Package v1alpha1 contains the DataService API.
//
// The group is mimir.siliconsaga.org, matching KafkaCluster and ValkeyCluster.
// The older Postgres/MySQL/Mongo claims still sit on the placeholder
// database.example.org group; moving them is tracked separately.
//
// +kubebuilder:object:generate=true
// +groupName=mimir.siliconsaga.org
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "mimir.siliconsaga.org", Version: "v1alpha1"}

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
