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
	// VRRPStateCondition is True while the router is VRRP master of at least
	// one group; its reason is the state keepalived reports.
	VRRPStateCondition = "VRRPMaster"
	// APIReachableCondition is True while the router's REST API answers with
	// the expected certificate.
	APIReachableCondition = "APIReachable"
)

// ValueSource is where a template value comes from. Exactly one field is set.
// +kubebuilder:validation:XValidation:rule="(has(self.value) ? 1 : 0) + (has(self.secretKeyRef) ? 1 : 0) + (has(self.configMapKeyRef) ? 1 : 0) == 1",message="exactly one of value, secretKeyRef and configMapKeyRef is required"
type ValueSource struct {
	// Value is a literal.
	// +kubebuilder:validation:MaxLength=65536
	// +optional
	Value *string `json:"value,omitempty"`
	// SecretKeyRef reads the value from a Secret in the same namespace.
	// +optional
	SecretKeyRef *corev1.SecretKeySelector `json:"secretKeyRef,omitempty"`
	// ConfigMapKeyRef reads the value from a ConfigMap in the same namespace.
	// +optional
	ConfigMapKeyRef *corev1.ConfigMapKeySelector `json:"configMapKeyRef,omitempty"`
}

// Value is a named input of the configuration template, available there as
// {{ .Values.<name> }}.
type Value struct {
	// Name of the value.
	// +kubebuilder:validation:Pattern=`^[A-Za-z_][A-Za-z0-9_]*$`
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`
	// ValueSource of the value.
	ValueSource `json:",inline"`
}

// File is a file placed on the router when it is first booted, below /config,
// the directory VyOS keeps across upgrades. Files are part of the bootstrap
// data: changing one replaces the router.
type File struct {
	// Path of the file. It has to be below /config.
	// +kubebuilder:validation:Pattern=`^/config/[A-Za-z0-9._/-]+$`
	// +kubebuilder:validation:MaxLength=255
	Path string `json:"path"`
	// Permissions as an octal string.
	// +kubebuilder:validation:Pattern=`^0[0-7]{3}$`
	// +kubebuilder:default="0600"
	// +optional
	Permissions string `json:"permissions,omitempty"`
	// ValueSource of the content. It is rendered as a template with the same
	// data as the configuration.
	ValueSource `json:",inline"`
}

// SlotSpec is the part of a configuration that belongs to one slot.
type SlotSpec struct {
	// Values for the router in this slot. They are available to the
	// templates like the shared values, and win over a shared value of the
	// same name.
	// +listType=map
	// +listMapKey=name
	// +optional
	Values []Value `json:"values,omitempty"`
}

// ManagementSpec says how the operator talks to the router.
type ManagementSpec struct {
	// Port the router's HTTPS API listens on.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:default=443
	// +optional
	Port int32 `json:"port,omitempty"`
	// AddressType of the machine address the operator connects to.
	// +kubebuilder:default=ExternalIP
	// +optional
	AddressType corev1alpha1.AddressType `json:"addressType,omitempty"`
	// AllowedSources restricts the API on the router itself to these source
	// ranges, in addition to whatever firewall the infrastructure has. Leave
	// it empty when the operator's address changes.
	// +listType=set
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=43
	// +kubebuilder:validation:items:Pattern=`^[0-9A-Fa-f:.]+(/[0-9]{1,3})?$`
	// +optional
	AllowedSources []string `json:"allowedSources,omitempty"`
	// ConfirmTimeoutMinutes is how long the router waits for the operator to
	// confirm a change before it reverts it by itself. A change that cuts the
	// operator off is undone after this time.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=60
	// +kubebuilder:default=2
	// +optional
	ConfirmTimeoutMinutes int32 `json:"confirmTimeoutMinutes,omitempty"`
}

// HostSpec describes the host the VyOS container runs on.
type HostSpec struct {
	// PublicInterface is the name the host's public network interface gets,
	// and the name the configuration refers to it by.
	// +kubebuilder:validation:Pattern=`^eth[0-9]+$`
	// +kubebuilder:default=eth0
	// +optional
	PublicInterface string `json:"publicInterface,omitempty"`
	// PrivateInterface is the name the host's private network interface gets.
	// +kubebuilder:validation:Pattern=`^eth[0-9]+$`
	// +kubebuilder:default=eth1
	// +optional
	PrivateInterface string `json:"privateInterface,omitempty"`
}

// VyOSConfigSpec describes a VyOS router: what it boots and what it runs.
//
// Image, Files and Host go into the data the instance is created with, so a
// change to them replaces the router. Commands, Values and Management are
// applied to the running router.
type VyOSConfigSpec struct {
	// Image is the VyOS container image, by digest for a reproducible router.
	// It is written into a systemd unit file on the host, so it is held to
	// the characters an image reference is made of.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9._/:@-]*$`
	Image string `json:"image"`
	// Commands is the router's configuration as VyOS "set" commands, one per
	// line; empty lines and lines starting with # are ignored. It is a Go
	// template, see docs/configuration.md for the data it is rendered with.
	// The router is made to match it exactly: what is not listed is deleted,
	// apart from what the operator itself needs to stay in contact.
	// +kubebuilder:validation:MaxLength=524288
	// +optional
	Commands string `json:"commands,omitempty"`
	// Values available to the templates.
	// +listType=map
	// +listMapKey=name
	// +optional
	Values []Value `json:"values,omitempty"`
	// Slots holds what differs between the routers of a group whose
	// RouterDeployment uses the Slots strategy: the first entry is for the
	// router in slot 0, the second for the one in slot 1, and so on. A
	// router's own tunnel address or key goes here. If this is set there has
	// to be an entry for every slot.
	// +kubebuilder:validation:MaxItems=32
	// +listType=atomic
	// +optional
	Slots []SlotSpec `json:"slots,omitempty"`
	// Files placed below /config at first boot.
	// +listType=map
	// +listMapKey=path
	// +optional
	Files []File `json:"files,omitempty"`
	// Management connection to the router.
	// +optional
	Management ManagementSpec `json:"management,omitempty"`
	// Host the container runs on.
	// +optional
	Host HostSpec `json:"host,omitempty"`
}

// VyOSConfigInitialization reports the one-time creation of bootstrap data.
// +kubebuilder:validation:MinProperties=1
type VyOSConfigInitialization struct {
	// DataSecretCreated is true once the bootstrap Secret exists.
	// +optional
	DataSecretCreated *bool `json:"dataSecretCreated,omitempty"`
}

// VyOSConfigStatus is the observed state of a VyOSConfig.
type VyOSConfigStatus struct {
	// DataSecretName is the Secret holding the instance's user data under
	// the key "value".
	// +optional
	DataSecretName string `json:"dataSecretName,omitempty"`
	// Initialization of the bootstrap data.
	// +optional
	Initialization *VyOSConfigInitialization `json:"initialization,omitempty"`
	// AppliedHash identifies the rendered configuration the router runs.
	// +optional
	AppliedHash string `json:"appliedHash,omitempty"`
	// RunningHash identifies the configuration the router was running right
	// after the last apply. VyOS rewrites some values when it stores them,
	// so it differs from AppliedHash; a router whose configuration no longer
	// hashes to it has been changed by someone else.
	// +optional
	RunningHash string `json:"runningHash,omitempty"`
	// LastAppliedTime is when the configuration was last changed.
	// +optional
	LastAppliedTime *metav1.Time `json:"lastAppliedTime,omitempty"`
	// FailedHash identifies a rendered configuration the router rejected.
	// It is not tried again until it changes or some time has passed.
	// +optional
	FailedHash string `json:"failedHash,omitempty"`
	// LastFailureTime is when FailedHash was last tried.
	// +optional
	LastFailureTime *metav1.Time `json:"lastFailureTime,omitempty"`
	// Version is the VyOS version the router reports.
	// +optional
	Version string `json:"version,omitempty"`
	// ObservedGeneration is the generation this status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Conditions: Ready, ConfigApplied, Healthy, APIReachable, VRRPMaster,
	// Drained.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// VyOSConfig boots a VyOS router and keeps it configured.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:categories=router-api,shortName=vc
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Applied",type=string,JSONPath=`.status.conditions[?(@.type=="ConfigApplied")].status`
// +kubebuilder:printcolumn:name="VRRP",type=string,JSONPath=`.status.conditions[?(@.type=="VRRPMaster")].reason`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.status.version`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type VyOSConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VyOSConfigSpec   `json:"spec"`
	Status VyOSConfigStatus `json:"status,omitempty"`
}

// VyOSConfigList is a list of VyOSConfigs.
// +kubebuilder:object:root=true
type VyOSConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VyOSConfig `json:"items"`
}

// VyOSConfigTemplateResource is the config a template stamps out.
type VyOSConfigTemplateResource struct {
	// Metadata copied to every VyOSConfig.
	// +optional
	ObjectMeta corev1alpha1.ObjectMeta `json:"metadata,omitempty"`
	// Spec of the configs.
	Spec VyOSConfigSpec `json:"spec"`
}

// VyOSConfigTemplateSpec wraps the config to stamp out.
type VyOSConfigTemplateSpec struct {
	// Template of the configs.
	Template VyOSConfigTemplateResource `json:"template"`
}

// VyOSConfigTemplateStatus is the observed state of a VyOSConfigTemplate.
type VyOSConfigTemplateStatus struct {
	// ReplacementHash covers the fields a router cannot change while it
	// runs. Core controllers replace routers when it changes and update them
	// in place when only the rest of the template did.
	// +optional
	ReplacementHash string `json:"replacementHash,omitempty"`
	// ObservedGeneration is the generation this status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// VyOSConfigTemplate is the mould VyOSConfigs are created from, and the place
// to edit the configuration of a group of routers.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:categories=router-api,shortName=vct
// +kubebuilder:printcolumn:name="Image",type=string,JSONPath=`.spec.template.spec.image`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type VyOSConfigTemplate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VyOSConfigTemplateSpec   `json:"spec"`
	Status VyOSConfigTemplateStatus `json:"status,omitempty"`
}

// VyOSConfigTemplateList is a list of VyOSConfigTemplates.
// +kubebuilder:object:root=true
type VyOSConfigTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VyOSConfigTemplate `json:"items"`
}

func init() {
	SchemeBuilder.Register(&VyOSConfig{}, &VyOSConfigList{}, &VyOSConfigTemplate{}, &VyOSConfigTemplateList{})
}
