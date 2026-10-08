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

// RouterFinalizer keeps a Router until its machine and config are gone.
const RouterFinalizer = "router.hauke.cloud/router"

// RouterPhase is a one-word summary of where a router is in its life.
// +kubebuilder:validation:Enum=Pending;Provisioning;Configuring;Ready;Degraded;Draining;Deleting;Unknown
type RouterPhase string

const (
	// RouterPhasePending means nothing has been created yet.
	RouterPhasePending RouterPhase = "Pending"
	// RouterPhaseProvisioning means the instance is being created.
	RouterPhaseProvisioning RouterPhase = "Provisioning"
	// RouterPhaseConfiguring means the instance exists and its configuration
	// has not been applied yet.
	RouterPhaseConfiguring RouterPhase = "Configuring"
	// RouterPhaseReady means the router runs its configuration and is healthy.
	RouterPhaseReady RouterPhase = "Ready"
	// RouterPhaseDegraded means the router was ready and no longer is.
	RouterPhaseDegraded RouterPhase = "Degraded"
	// RouterPhaseDraining means the router is giving up its addresses.
	RouterPhaseDraining RouterPhase = "Draining"
	// RouterPhaseDeleting means the router is being removed.
	RouterPhaseDeleting RouterPhase = "Deleting"
	// RouterPhaseUnknown means the state could not be determined.
	RouterPhaseUnknown RouterPhase = "Unknown"
)

// RouterSpec ties an instance to the configuration it runs.
type RouterSpec struct {
	// MachineRef names the RouterMachine this router runs on.
	MachineRef LocalObjectReference `json:"machineRef"`
	// ConfigRef names the config provider object that boots and configures
	// the router, for example a VyOSConfig.
	ConfigRef ContractReference `json:"configRef"`
}

// RouterStatus is the observed state of a Router.
type RouterStatus struct {
	// Phase summarises the router's state.
	// +optional
	Phase RouterPhase `json:"phase,omitempty"`
	// Addresses of the instance, copied from the machine so that config
	// providers have one place to read them.
	// +listType=atomic
	// +optional
	Addresses []MachineAddress `json:"addresses,omitempty"`
	// Version is the software version the router reports.
	// +optional
	Version string `json:"version,omitempty"`
	// ObservedGeneration is the generation this status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Conditions: Ready, MachineReady, BootstrapReady, ConfigApplied, Healthy,
	// Drained, Paused.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Router is one router: an instance and the configuration it runs. It is the
// object to look at to know whether a router works.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:categories=router-api,shortName=rt
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.status.version`
// +kubebuilder:printcolumn:name="Address",type=string,JSONPath=`.status.addresses[?(@.type=="ExternalIP")].address`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Router struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RouterSpec   `json:"spec"`
	Status RouterStatus `json:"status,omitempty"`
}

// RouterList is a list of Routers.
// +kubebuilder:object:root=true
type RouterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Router `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Router{}, &RouterList{})
}
