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

const (
	// ExposeAnnotation on a Gateway (gateway.networking.k8s.io) asks for the
	// Gateway's addresses and listener ports to be let through by the routers.
	// Its value names a RouterExposure, as "<namespace>/<name>", or as
	// "<name>" for one in the Gateway's own namespace. The RouterExposure
	// decides whether the Gateway's namespace may ask.
	ExposeAnnotation = "router.hauke.cloud/expose"

	// AllNamespaces in a RouterExposure's list of namespaces allows every
	// namespace.
	AllNamespaces = "*"
)

// ExposedProtocol is a transport protocol the routers can let through by
// port.
// +kubebuilder:validation:Enum=tcp;udp
type ExposedProtocol string

const (
	// ExposedTCP is TCP.
	ExposedTCP ExposedProtocol = "tcp"
	// ExposedUDP is UDP.
	ExposedUDP ExposedProtocol = "udp"
)

// ExposedPort is one port of one protocol.
type ExposedPort struct {
	// Protocol of the port.
	Protocol ExposedProtocol `json:"protocol"`
	// Port number.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`
}

// ExposedEndpoint is one address and what is to be let through to it.
type ExposedEndpoint struct {
	// Address is an IPv4 or IPv6 address.
	Address string `json:"address"`
	// Ports to let through to the address, sorted by protocol and number.
	// +listType=atomic
	Ports []ExposedPort `json:"ports"`
}

// ExposedGateway says what became of one Gateway that asked to be exposed.
type ExposedGateway struct {
	// Namespace of the Gateway.
	Namespace string `json:"namespace"`
	// Name of the Gateway.
	Name string `json:"name"`
	// Exposed is true if at least one address of the Gateway is part of
	// the endpoints.
	Exposed bool `json:"exposed"`
	// Message says why the Gateway is not exposed, or what of it is left
	// out.
	// +optional
	Message string `json:"message,omitempty"`
}

// GatewaySource says which Gateways a RouterExposure listens to.
type GatewaySource struct {
	// Namespaces whose Gateways may name this object in their
	// router.hauke.cloud/expose annotation, next to the namespace of this
	// object, which always may. "*" allows every namespace. A Gateway
	// elsewhere that names this object is reported in the status and
	// otherwise ignored: being able to create a Gateway somewhere must not
	// be enough to open ports on the routers.
	// +kubebuilder:validation:MaxItems=256
	// +kubebuilder:validation:items:MaxLength=63
	// +listType=set
	// +optional
	Namespaces []string `json:"namespaces,omitempty"`
}

// RouterExposureSpec says where the list of what the routers let through
// comes from.
type RouterExposureSpec struct {
	// Gateways that may contribute.
	// +optional
	Gateways GatewaySource `json:"gateways,omitempty"`
}

// RouterExposureStatus is what the routers are to let through. Providers read
// it; nothing in it is specific to one of them.
type RouterExposureStatus struct {
	// Endpoints are the addresses of the Gateways with the ports of their
	// listeners, sorted by address.
	// +listType=map
	// +listMapKey=address
	// +optional
	Endpoints []ExposedEndpoint `json:"endpoints,omitempty"`
	// Ports is every port of Endpoints once, for a firewall that cannot
	// tell destination addresses apart.
	// +listType=atomic
	// +optional
	Ports []ExposedPort `json:"ports,omitempty"`
	// Gateways that name this object, sorted by namespace and name.
	// +listType=atomic
	// +optional
	Gateways []ExposedGateway `json:"gateways,omitempty"`
	// ObservedGeneration is the generation this status was computed from.
	// It is 0 until the Gateways have been looked at for the first time,
	// and providers do not act on a status that has never been computed.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Conditions: Ready.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// RouterExposure collects what a group of routers lets through from the
// Gateways that ask for it, instead of from a list kept by hand. A firewall
// or a router configuration refers to it and follows it.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:categories=router-api,shortName=rex
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Addresses",type=string,JSONPath=`.status.endpoints[*].address`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type RouterExposure struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +optional
	Spec   RouterExposureSpec   `json:"spec,omitempty"`
	Status RouterExposureStatus `json:"status,omitempty"`
}

// RouterExposureList is a list of RouterExposures.
// +kubebuilder:object:root=true
type RouterExposureList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RouterExposure `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RouterExposure{}, &RouterExposureList{})
}
