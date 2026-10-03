// Package v1alpha1 contains the BackupPolicy API.
// +kubebuilder:object:generate=true
// +groupName=dbbackup.equinoxwn.github.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the API group and version of every type in this package.
	GroupVersion = schema.GroupVersion{Group: "dbbackup.equinoxwn.github.io", Version: "v1alpha1"}

	// SchemeBuilder registers the types with a runtime scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
