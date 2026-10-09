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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/hauke-cloud/router-api/api/core/v1alpha1"
)

const (
	// HetznerMachineFinalizer keeps a HetznerMachine until its server is gone.
	HetznerMachineFinalizer = "infrastructure.router.hauke.cloud/hetznermachine"
	// ProviderIDPrefix is the scheme of a Hetzner Cloud provider ID; the
	// server ID follows it.
	ProviderIDPrefix = "hcloud://"
)

// HetznerMachineSpec describes a Hetzner Cloud server.
type HetznerMachineSpec struct {
	// NetworkRef names the HetznerRouterNetwork the server belongs to.
	NetworkRef corev1.LocalObjectReference `json:"networkRef"`
	// ServerType, for example cx23.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	ServerType string `json:"serverType"`
	// Location, for example fsn1.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Location string `json:"location"`
	// Image the server is created from: the name of a system image or the ID
	// of a snapshot.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:default="ubuntu-24.04"
	// +optional
	Image string `json:"image,omitempty"`
	// EnableIPv4 gives the server a public IPv4 address.
	// +kubebuilder:default=true
	// +optional
	EnableIPv4 *bool `json:"enableIPv4,omitempty"`
	// EnableIPv6 gives the server a public IPv6 network.
	// +kubebuilder:default=true
	// +optional
	EnableIPv6 *bool `json:"enableIPv6,omitempty"`
	// PrimaryIPv4BySlot gives the router in each slot a public address that
	// stays when the router is replaced: the names of existing Hetzner
	// Primary IPs, the first for slot 0, the second for slot 1, and so on.
	// It needs a RouterDeployment with the Slots strategy.
	//
	// The Primary IPs are yours: create them in the location of the servers
	// and with auto-delete off, or Hetzner deletes them with the first
	// server they were assigned to. They are neither created nor deleted
	// here.
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=63
	// +listType=atomic
	// +optional
	PrimaryIPv4BySlot []string `json:"primaryIPv4BySlot,omitempty"`
	// ProviderID is hcloud://<server id>, set once the server exists.
	// +kubebuilder:validation:MaxLength=512
	// +optional
	ProviderID string `json:"providerID,omitempty"`
}

// HetznerMachineInitialization reports the one-time provisioning of a server.
// +kubebuilder:validation:MinProperties=1
type HetznerMachineInitialization struct {
	// Provisioned is true once the server has been created and is running.
	// +optional
	Provisioned *bool `json:"provisioned,omitempty"`
}

// HetznerMachineStatus is the observed state of a HetznerMachine.
type HetznerMachineStatus struct {
	// Initialization of the server.
	// +optional
	Initialization *HetznerMachineInitialization `json:"initialization,omitempty"`
	// Addresses of the server.
	// +listType=atomic
	// +optional
	Addresses []corev1alpha1.MachineAddress `json:"addresses,omitempty"`
	// FailureDomain is the location the server is in.
	// +optional
	FailureDomain string `json:"failureDomain,omitempty"`
	// ServerStatus is the status Hetzner reports, for example running.
	// +optional
	ServerStatus string `json:"serverStatus,omitempty"`
	// LastRemediation is the value of the remediation annotation that was
	// acted on last.
	// +optional
	LastRemediation string `json:"lastRemediation,omitempty"`
	// ObservedGeneration is the generation this status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Conditions: Ready.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// HetznerMachine is a Hetzner Cloud server that hosts a router.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:categories=router-api,shortName=hm
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.serverType`
// +kubebuilder:printcolumn:name="Location",type=string,JSONPath=`.spec.location`
// +kubebuilder:printcolumn:name="Server",type=string,JSONPath=`.status.serverStatus`
// +kubebuilder:printcolumn:name="ProviderID",type=string,JSONPath=`.spec.providerID`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type HetznerMachine struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HetznerMachineSpec   `json:"spec"`
	Status HetznerMachineStatus `json:"status,omitempty"`
}

// HetznerMachineList is a list of HetznerMachines.
// +kubebuilder:object:root=true
type HetznerMachineList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HetznerMachine `json:"items"`
}

// HetznerMachineTemplateResource is the machine a template stamps out.
type HetznerMachineTemplateResource struct {
	// Metadata copied to every HetznerMachine.
	// +optional
	ObjectMeta corev1alpha1.ObjectMeta `json:"metadata,omitempty"`
	// Spec of the machines.
	Spec HetznerMachineSpec `json:"spec"`
}

// HetznerMachineTemplateSpec wraps the machine to stamp out.
type HetznerMachineTemplateSpec struct {
	// Template of the machines.
	Template HetznerMachineTemplateResource `json:"template"`
}

// HetznerMachineTemplate is the mould HetznerMachines are created from. Any
// change to it replaces the routers built from it.
// +kubebuilder:object:root=true
// +kubebuilder:resource:categories=router-api,shortName=hmt
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.template.spec.serverType`
// +kubebuilder:printcolumn:name="Location",type=string,JSONPath=`.spec.template.spec.location`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type HetznerMachineTemplate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec HetznerMachineTemplateSpec `json:"spec"`
}

// HetznerMachineTemplateList is a list of HetznerMachineTemplates.
// +kubebuilder:object:root=true
type HetznerMachineTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HetznerMachineTemplate `json:"items"`
}

func init() {
	SchemeBuilder.Register(&HetznerMachine{}, &HetznerMachineList{}, &HetznerMachineTemplate{}, &HetznerMachineTemplateList{})
}
