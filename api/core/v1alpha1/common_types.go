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
)

const (
	// DeploymentNameLabel is set on everything a RouterDeployment creates, and
	// is how the routers of one group find each other.
	DeploymentNameLabel = "router.hauke.cloud/deployment-name"
	// SetNameLabel is set on everything a RouterSet creates.
	SetNameLabel = "router.hauke.cloud/set-name"
	// RouterNameLabel is set on the objects that make up one router.
	RouterNameLabel = "router.hauke.cloud/router-name"
	// SlotLabel is the slot of a router within its group, "0", "1", ...,
	// on the Router and on everything that belongs to it. Only groups whose
	// strategy is Slots have it. A slot outlives the router that holds it:
	// the replacement of the router in slot 1 is again the router in slot 1,
	// and providers hang on it whatever must not change with a replacement,
	// such as a public address or a key.
	SlotLabel = "router.hauke.cloud/slot"
	// TemplateHashLabel carries the hash of the template a RouterSet was
	// created from, the way pod-template-hash does for a ReplicaSet.
	TemplateHashLabel = "router.hauke.cloud/template-hash"

	// PausedAnnotation stops every controller from acting on the object it is
	// set on. Any value pauses.
	PausedAnnotation = "router.hauke.cloud/paused"
	// ClonedFromNameAnnotation and ClonedFromGroupKindAnnotation record which
	// template a provider object was created from.
	ClonedFromNameAnnotation = "router.hauke.cloud/cloned-from-name"
	// ClonedFromGroupKindAnnotation is the Kind.group of that template.
	ClonedFromGroupKindAnnotation = "router.hauke.cloud/cloned-from-groupkind"
	// RevisionAnnotation is the revision of a RouterSet within its deployment.
	RevisionAnnotation = "router.hauke.cloud/revision"

	// DrainAnnotation asks the config provider to take the router out of
	// service without stopping it: give up VRRP mastership so that addresses
	// move to a peer. The provider answers with the Drained condition.
	DrainAnnotation = "router.hauke.cloud/drain"
	// RemediationAnnotation asks the infrastructure provider to recover the
	// instance. The value is a RemediationAction; the provider answers by
	// copying the value to its status.lastRemediation.
	RemediationAnnotation = "router.hauke.cloud/remediation"
)

// ContractReference points at a provider object in the same namespace. Core
// controllers never import a provider's types: they read and write the object
// through the fields the provider contract names (docs/provider-contract.md).
type ContractReference struct {
	// APIGroup of the referenced object, without a version. The served
	// version is discovered from the CustomResourceDefinition.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	APIGroup string `json:"apiGroup"`
	// Kind of the referenced object.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Kind string `json:"kind"`
	// Name of the referenced object.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// AddressType says what kind of address a MachineAddress is.
// +kubebuilder:validation:Enum=Hostname;ExternalIP;InternalIP;ExternalDNS;InternalDNS
type AddressType string

const (
	// AddressHostname is the instance's host name.
	AddressHostname AddressType = "Hostname"
	// AddressExternalIP is an address reachable from the internet.
	AddressExternalIP AddressType = "ExternalIP"
	// AddressInternalIP is an address in a private network.
	AddressInternalIP AddressType = "InternalIP"
	// AddressExternalDNS is a public DNS name.
	AddressExternalDNS AddressType = "ExternalDNS"
	// AddressInternalDNS is a DNS name that resolves in a private network.
	AddressInternalDNS AddressType = "InternalDNS"
)

// MachineAddress is one address of an instance.
type MachineAddress struct {
	// Type of the address.
	Type AddressType `json:"type"`
	// Address is the IP address or name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	Address string `json:"address"`
}

// RemediationAction is something an infrastructure provider can do to an
// instance that stopped working.
// +kubebuilder:validation:Enum=Reboot
type RemediationAction string

// RemediationReboot power-cycles the instance.
const RemediationReboot RemediationAction = "Reboot"

// Condition types and reasons shared by the core kinds.
const (
	// ReadyCondition summarises an object: True means it does what its spec
	// asks.
	ReadyCondition = "Ready"
	// PausedCondition is True while the object carries the paused annotation.
	PausedCondition = "Paused"
	// InfrastructureReadyCondition mirrors the infrastructure object's Ready.
	InfrastructureReadyCondition = "InfrastructureReady"
	// BootstrapReadyCondition is True once the config provider has published
	// the data the instance boots with.
	BootstrapReadyCondition = "BootstrapReady"
	// MachineReadyCondition mirrors the RouterMachine's Ready on a Router.
	MachineReadyCondition = "MachineReady"
	// ConfigAppliedCondition is True while the router runs the configuration
	// its config object currently describes. It is not part of Ready, see
	// Router.
	ConfigAppliedCondition = "ConfigApplied"
	// HealthyCondition is True while the config provider can reach the router
	// and finds it working.
	HealthyCondition = "Healthy"
	// DrainedCondition is True once a router asked to drain no longer holds
	// any address a peer could hold.
	DrainedCondition = "Drained"
	// AvailableCondition is True on a RouterSet or RouterDeployment while
	// enough of its routers are ready.
	AvailableCondition = "Available"
	// RollingOutCondition is True on a RouterDeployment while routers are
	// being replaced or reconfigured.
	RollingOutCondition = "RollingOut"
	// RemediationAllowedCondition is False on a RouterHealthCheck while too
	// many routers are unhealthy to act on any of them.
	RemediationAllowedCondition = "RemediationAllowed"
)

// LocalObjectReference names an object of a known kind in the same namespace.
type LocalObjectReference = corev1.LocalObjectReference
