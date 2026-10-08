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

// HetznerRouterNetworkFinalizer keeps a HetznerRouterNetwork until the
// Hetzner resources it created are gone.
const HetznerRouterNetworkFinalizer = "infrastructure.router.hauke.cloud/hetznerrouternetwork"

// SecretKeyReference names one key of a Secret in the same namespace.
type SecretKeyReference struct {
	// Name of the Secret.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
	// Key in the Secret.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:default=token
	// +optional
	Key string `json:"key,omitempty"`
}

// ManagementSource is where the operator's connections to the routers come
// from. Exactly one of CIDR and Hostname is set.
// +kubebuilder:validation:XValidation:rule="has(self.cidr) != has(self.hostname)",message="exactly one of cidr and hostname is required"
type ManagementSource struct {
	// CIDR is a fixed source range.
	// +kubebuilder:validation:MaxLength=43
	// +optional
	CIDR string `json:"cidr,omitempty"`
	// Hostname is resolved again and again, and the firewall follows the
	// answer. This is for an operator behind a connection whose address
	// changes, published through dynamic DNS.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	Hostname string `json:"hostname,omitempty"`
}

// FirewallProtocol is a protocol a Hetzner Cloud Firewall rule can match.
// +kubebuilder:validation:Enum=tcp;udp;icmp;esp;gre
type FirewallProtocol string

// FirewallRule opens the routers to inbound traffic. Anything not matched by
// a rule is dropped before it reaches a router.
type FirewallRule struct {
	// Description shown in the Hetzner console.
	// +kubebuilder:validation:MaxLength=255
	// +optional
	Description string `json:"description,omitempty"`
	// Protocol to allow.
	Protocol FirewallProtocol `json:"protocol"`
	// Port or port range ("443", "1024-2048"). Required for tcp and udp.
	// +kubebuilder:validation:Pattern=`^(any|[0-9]{1,5}(-[0-9]{1,5})?)$`
	// +optional
	Port string `json:"port,omitempty"`
	// SourceCIDRs the traffic may come from. Defaults to everywhere.
	// +listType=set
	// +optional
	SourceCIDRs []string `json:"sourceCIDRs,omitempty"`
}

// HetznerFirewall describes the Hetzner Cloud Firewall in front of the
// routers.
type HetznerFirewall struct {
	// ManagementPort is the port of the routers' management API. It is opened
	// to ManagementSources and to nothing else.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:default=443
	// +optional
	ManagementPort int32 `json:"managementPort,omitempty"`
	// ManagementSources the operator connects from.
	// +kubebuilder:validation:MinItems=1
	// +listType=atomic
	ManagementSources []ManagementSource `json:"managementSources"`
	// Rules for everything else the routers should receive: VPN endpoints,
	// forwarded services, ICMP.
	// +listType=atomic
	// +optional
	Rules []FirewallRule `json:"rules,omitempty"`
}

// HetznerNetworkAttachment names the existing Hetzner Cloud Network the
// routers join. The network is not created or deleted by this provider: it
// outlives any one router group.
type HetznerNetworkAttachment struct {
	// Name of the network.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`
}

// HetznerPlacementGroup keeps the routers on different physical hosts.
type HetznerPlacementGroup struct {
	// Enabled creates a spread placement group for the routers.
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
}

// HetznerRouterNetworkSpec is what the routers of a group share at Hetzner.
type HetznerRouterNetworkSpec struct {
	// TokenSecretRef is the Hetzner Cloud API token, with read and write
	// access to the project.
	TokenSecretRef SecretKeyReference `json:"tokenSecretRef"`
	// Network the routers are attached to. VRRP between them runs over it.
	Network HetznerNetworkAttachment `json:"network"`
	// Firewall in front of the routers.
	Firewall HetznerFirewall `json:"firewall"`
	// PlacementGroup for the routers.
	// +optional
	PlacementGroup HetznerPlacementGroup `json:"placementGroup,omitempty"`
	// SSHKeys are the names of Hetzner SSH keys installed on the host, for
	// debugging. The operator itself never uses SSH.
	// +listType=set
	// +optional
	SSHKeys []string `json:"sshKeys,omitempty"`
}

// HetznerRouterNetworkStatus is the observed state of a HetznerRouterNetwork.
type HetznerRouterNetworkStatus struct {
	// NetworkID of the attached network.
	// +optional
	NetworkID int64 `json:"networkID,omitempty"`
	// FirewallID of the firewall this object owns.
	// +optional
	FirewallID int64 `json:"firewallID,omitempty"`
	// PlacementGroupID of the placement group this object owns.
	// +optional
	PlacementGroupID int64 `json:"placementGroupID,omitempty"`
	// ManagementCIDRs currently allowed to reach the management port, after
	// resolving host names.
	// +listType=set
	// +optional
	ManagementCIDRs []string `json:"managementCIDRs,omitempty"`
	// ObservedGeneration is the generation this status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Conditions: Ready.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// HetznerRouterNetwork is what the routers of a group share at Hetzner Cloud:
// the API token, the private network, the firewall and the placement group.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:categories=router-api,shortName=hrn
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Network",type=string,JSONPath=`.spec.network.name`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type HetznerRouterNetwork struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HetznerRouterNetworkSpec   `json:"spec"`
	Status HetznerRouterNetworkStatus `json:"status,omitempty"`
}

// HetznerRouterNetworkList is a list of HetznerRouterNetworks.
// +kubebuilder:object:root=true
type HetznerRouterNetworkList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HetznerRouterNetwork `json:"items"`
}

func init() {
	SchemeBuilder.Register(&HetznerRouterNetwork{}, &HetznerRouterNetworkList{})
}
