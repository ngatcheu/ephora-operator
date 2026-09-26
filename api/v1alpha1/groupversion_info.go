// Package v1alpha1 contains API Schema definitions for the ephora.io v1alpha1 API group.
// +kubebuilder:object:generate=true
// +groupName=ephora.io
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion is group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "ephora.io", Version: "v1alpha1"}

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
	// Plain apimachinery (not controller-runtime's scheme.Builder) keeps this
	// API package importable with minimal dependencies.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(GroupVersion, &PreviewEnvironment{}, &PreviewEnvironmentList{})
	metav1.AddToGroupVersion(scheme, GroupVersion)
	return nil
}
