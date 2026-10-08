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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RouterSetFinalizer keeps a RouterSet until its routers are gone.
const RouterSetFinalizer = "router.hauke.cloud/routerset"

// ObjectMeta is the part of an object's metadata a template may set.
type ObjectMeta struct {
	// Labels added to the created object.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`
	// Annotations added to the created object.
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
}

// RouterTemplateSpec describes the routers a RouterSet creates.
type RouterTemplateSpec struct {
	// Metadata copied to every Router.
	// +optional
	ObjectMeta `json:"metadata,omitempty"`
	// Spec of the routers.
	Spec RouterTemplate `json:"spec"`
}

// RouterTemplate names the provider templates one router is built from.
type RouterTemplate struct {
	// InfrastructureTemplateRef names the template the instance is created
	// from, for example a HetznerMachineTemplate. A change replaces routers.
	InfrastructureTemplateRef ContractReference `json:"infrastructureTemplateRef"`
	// ConfigTemplateRef names the template the configuration is created
	// from, for example a VyOSConfigTemplate. Whether a change replaces
	// routers or is applied to the running ones is the config provider's
	// call, see docs/provider-contract.md.
	ConfigTemplateRef ContractReference `json:"configTemplateRef"`
}

// RouterSetSpec describes a set of identical routers.
type RouterSetSpec struct {
	// Replicas is the number of routers.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=1
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`
	// Selector matches the routers of this set. It has to match the
	// template's labels.
	Selector metav1.LabelSelector `json:"selector"`
	// Template of the routers.
	Template RouterTemplateSpec `json:"template"`
	// MinReadySeconds a router has to stay ready before it counts as
	// available.
	// +kubebuilder:validation:Minimum=0
	// +optional
	MinReadySeconds int32 `json:"minReadySeconds,omitempty"`
}

// RouterSetStatus is the observed state of a RouterSet.
type RouterSetStatus struct {
	// Selector in string form, for the scale subresource.
	// +optional
	Selector string `json:"selector,omitempty"`
	// Replicas is the number of routers that exist.
	// +optional
	Replicas int32 `json:"replicas"`
	// ReadyReplicas is the number of routers that are ready.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas"`
	// AvailableReplicas is the number of routers that have been ready for
	// MinReadySeconds.
	// +optional
	AvailableReplicas int32 `json:"availableReplicas"`
	// UpToDateReplicas is the number of routers whose configuration matches
	// the current config template.
	// +optional
	UpToDateReplicas int32 `json:"upToDateReplicas"`
	// ObservedGeneration is the generation this status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Conditions: Available, Paused.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// RouterSet keeps a number of identical routers running. It is usually
// created by a RouterDeployment.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.replicas,selectorpath=.status.selector
// +kubebuilder:resource:categories=router-api,shortName=rs
// +kubebuilder:printcolumn:name="Desired",type=integer,JSONPath=`.spec.replicas`
// +kubebuilder:printcolumn:name="Current",type=integer,JSONPath=`.status.replicas`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Available",type=integer,JSONPath=`.status.availableReplicas`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type RouterSet struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RouterSetSpec   `json:"spec"`
	Status RouterSetStatus `json:"status,omitempty"`
}

// RouterSetList is a list of RouterSets.
// +kubebuilder:object:root=true
type RouterSetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RouterSet `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RouterSet{}, &RouterSetList{})
}
