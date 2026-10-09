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
	"k8s.io/apimachinery/pkg/util/intstr"
)

// RouterDeploymentStrategyType is the way routers are replaced.
// +kubebuilder:validation:Enum=Surge;Slots
type RouterDeploymentStrategyType string

const (
	// SurgeStrategy builds a replacement before it takes a router away. The
	// group is never short, and its routers are interchangeable: whatever
	// identifies the group to the outside has to be able to move between
	// them.
	SurgeStrategy RouterDeploymentStrategyType = "Surge"
	// SlotsStrategy gives every router a slot, 0 up to replicas-1, that
	// survives its replacement, so that each router can have something of
	// its own that stays: a public address, a key, a tunnel. A router is
	// replaced in its slot, which means it is removed before its successor
	// is built, and the group is one router short meanwhile.
	SlotsStrategy RouterDeploymentStrategyType = "Slots"
)

// RouterDeploymentStrategy says how routers are replaced. Either way it is
// one after the other: replacing every router at once is never what a router
// group wants.
type RouterDeploymentStrategy struct {
	// Type of the strategy.
	// +kubebuilder:default=Surge
	// +optional
	Type RouterDeploymentStrategyType `json:"type,omitempty"`
	// RollingUpdate parameters of the Surge strategy. With Slots they are
	// not used: there is no slot for an extra router, and one router at a
	// time is unavailable.
	// +optional
	RollingUpdate RollingUpdate `json:"rollingUpdate,omitempty"`
}

// RollingUpdate bounds how far a rollout may stray from the desired number of
// routers.
type RollingUpdate struct {
	// MaxSurge is how many routers may exist beyond Replicas during a
	// rollout, as a number or a percentage.
	// +kubebuilder:default=1
	// +kubebuilder:validation:XIntOrString
	// +optional
	MaxSurge *intstr.IntOrString `json:"maxSurge,omitempty"`
	// MaxUnavailable is how many routers may be unavailable during a
	// rollout, as a number or a percentage. The default of 0 means a router
	// is only taken out of service once its replacement is available.
	// +kubebuilder:default=0
	// +kubebuilder:validation:XIntOrString
	// +optional
	MaxUnavailable *intstr.IntOrString `json:"maxUnavailable,omitempty"`
}

// RouterDeploymentSpec describes a group of routers that back each other up.
type RouterDeploymentSpec struct {
	// Replicas is the number of routers. Two or more are needed for a
	// rollout or a failure to go unnoticed.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=2
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`
	// Selector matches the routers of this deployment. It has to match the
	// template's labels.
	Selector metav1.LabelSelector `json:"selector"`
	// Template of the routers.
	Template RouterTemplateSpec `json:"template"`
	// Strategy for replacing routers.
	// +optional
	Strategy RouterDeploymentStrategy `json:"strategy,omitempty"`
	// MinReadySeconds a router has to stay ready before it counts as
	// available. Give VRRP and routing protocols time to settle here.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=30
	// +optional
	MinReadySeconds *int32 `json:"minReadySeconds,omitempty"`
	// RevisionHistoryLimit is the number of scaled-down RouterSets kept.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=2
	// +optional
	RevisionHistoryLimit *int32 `json:"revisionHistoryLimit,omitempty"`
	// Paused stops rollouts. Existing routers keep being reconciled.
	// +optional
	Paused bool `json:"paused,omitempty"`
}

// RouterDeploymentStatus is the observed state of a RouterDeployment.
type RouterDeploymentStatus struct {
	// Selector in string form, for the scale subresource.
	// +optional
	Selector string `json:"selector,omitempty"`
	// Replicas is the number of routers that exist, across all sets.
	// +optional
	Replicas int32 `json:"replicas"`
	// UpdatedReplicas is the number of routers of the current revision.
	// +optional
	UpdatedReplicas int32 `json:"updatedReplicas"`
	// ReadyReplicas is the number of routers that are ready.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas"`
	// AvailableReplicas is the number of routers that have been ready for
	// MinReadySeconds.
	// +optional
	AvailableReplicas int32 `json:"availableReplicas"`
	// ObservedGeneration is the generation this status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Conditions: Available, RollingOut, Paused.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// RouterDeployment is a group of routers that back each other up, and the
// way to change them without an outage.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.replicas,selectorpath=.status.selector
// +kubebuilder:resource:categories=router-api,shortName=rd
// +kubebuilder:printcolumn:name="Desired",type=integer,JSONPath=`.spec.replicas`
// +kubebuilder:printcolumn:name="Updated",type=integer,JSONPath=`.status.updatedReplicas`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Available",type=integer,JSONPath=`.status.availableReplicas`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type RouterDeployment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RouterDeploymentSpec   `json:"spec"`
	Status RouterDeploymentStatus `json:"status,omitempty"`
}

// RouterDeploymentList is a list of RouterDeployments.
// +kubebuilder:object:root=true
type RouterDeploymentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RouterDeployment `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RouterDeployment{}, &RouterDeploymentList{})
}
