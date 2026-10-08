/*
Copyright 2026 hauke.cloud.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package v1alpha1 contains the API types of the infrastructure.router.hauke.cloud API group.
// +kubebuilder:object:generate=true
// +groupName=infrastructure.router.hauke.cloud
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// SchemeGroupVersion is group version used to register these objects.
	// This name is used by applyconfiguration generators (e.g. controller-gen).
	SchemeGroupVersion = schema.GroupVersion{Group: "infrastructure.router.hauke.cloud", Version: "v1alpha1"}

	// GroupVersion is an alias for SchemeGroupVersion, for backward compatibility.
	GroupVersion = SchemeGroupVersion

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
	SchemeBuilder = &Builder{GroupVersion: SchemeGroupVersion}

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

// Builder collects the types of one group-version and registers them with a
// scheme. It is the same shape as controller-runtime's deprecated
// scheme.Builder, kept here so that importing the API types does not pull in
// the controller machinery.
// +kubebuilder:object:generate=false
type Builder struct {
	// GroupVersion the types belong to.
	GroupVersion schema.GroupVersion

	types []runtime.Object
}

// Register adds types to the builder.
func (b *Builder) Register(objects ...runtime.Object) *Builder {
	b.types = append(b.types, objects...)
	return b
}

// AddToScheme registers every type the builder holds.
func (b *Builder) AddToScheme(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(b.GroupVersion, b.types...)
	metav1.AddToGroupVersion(scheme, b.GroupVersion)
	return nil
}
