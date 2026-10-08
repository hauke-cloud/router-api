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

// RouterMachineFinalizer keeps a RouterMachine until its infrastructure object
// is gone.
const RouterMachineFinalizer = "router.hauke.cloud/routermachine"

// RouterMachinePhase is a one-word summary of where a machine is in its life.
// +kubebuilder:validation:Enum=Pending;Provisioning;Running;Deleting;Failed;Unknown
type RouterMachinePhase string

const (
	// RouterMachinePhasePending means the machine waits for its bootstrap data.
	RouterMachinePhasePending RouterMachinePhase = "Pending"
	// RouterMachinePhaseProvisioning means the infrastructure provider is
	// creating the instance.
	RouterMachinePhaseProvisioning RouterMachinePhase = "Provisioning"
	// RouterMachinePhaseRunning means the instance exists and is running.
	RouterMachinePhaseRunning RouterMachinePhase = "Running"
	// RouterMachinePhaseDeleting means the instance is being removed.
	RouterMachinePhaseDeleting RouterMachinePhase = "Deleting"
	// RouterMachinePhaseFailed means the provider reported a problem it will
	// not recover from by itself.
	RouterMachinePhaseFailed RouterMachinePhase = "Failed"
	// RouterMachinePhaseUnknown means the state could not be determined.
	RouterMachinePhaseUnknown RouterMachinePhase = "Unknown"
)

// RouterMachineSpec describes the instance a router runs on.
type RouterMachineSpec struct {
	// InfrastructureRef names the provider object that creates the instance,
	// for example a HetznerMachine.
	InfrastructureRef ContractReference `json:"infrastructureRef"`
	// Bootstrap says where the data the instance boots with comes from.
	// +optional
	Bootstrap RouterMachineBootstrap `json:"bootstrap,omitempty"`
	// ProviderID identifies the instance at the provider. It is copied from
	// the infrastructure object once the instance exists.
	// +kubebuilder:validation:MaxLength=512
	// +optional
	ProviderID string `json:"providerID,omitempty"`
}

// RouterMachineBootstrap is the machine's bootstrap data.
type RouterMachineBootstrap struct {
	// DataSecretName is the Secret holding the user data under the key
	// "value". The Router controller sets it once the config provider has
	// published the Secret; the infrastructure provider waits for it.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +optional
	DataSecretName *string `json:"dataSecretName,omitempty"`
}

// RouterMachineStatus is the observed state of a RouterMachine.
type RouterMachineStatus struct {
	// Phase summarises the machine's state.
	// +optional
	Phase RouterMachinePhase `json:"phase,omitempty"`
	// Addresses of the instance, as reported by the infrastructure provider.
	// +listType=atomic
	// +optional
	Addresses []MachineAddress `json:"addresses,omitempty"`
	// FailureDomain the instance was placed in.
	// +optional
	FailureDomain string `json:"failureDomain,omitempty"`
	// InfrastructureProvisioned is true once the provider reports the
	// instance as created. It does not go back to false.
	// +optional
	InfrastructureProvisioned bool `json:"infrastructureProvisioned,omitempty"`
	// LastRemediation is the remediation request the provider acted on last.
	// +optional
	LastRemediation string `json:"lastRemediation,omitempty"`
	// ObservedGeneration is the generation this status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Conditions: Ready, InfrastructureReady, Paused.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// RouterMachine is the instance a router runs on, independent of the provider
// that creates it.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:categories=router-api,shortName=rm
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="ProviderID",type=string,JSONPath=`.spec.providerID`
// +kubebuilder:printcolumn:name="Address",type=string,JSONPath=`.status.addresses[?(@.type=="ExternalIP")].address`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type RouterMachine struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RouterMachineSpec   `json:"spec"`
	Status RouterMachineStatus `json:"status,omitempty"`
}

// RouterMachineList is a list of RouterMachines.
// +kubebuilder:object:root=true
type RouterMachineList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RouterMachine `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RouterMachine{}, &RouterMachineList{})
}
