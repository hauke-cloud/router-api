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

// UnhealthyCondition is a condition that, held for long enough, makes a router
// unhealthy.
type UnhealthyCondition struct {
	// Type of the Router condition to watch.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=316
	Type string `json:"type"`
	// Status the condition has to have.
	// +kubebuilder:validation:Enum="True";"False";Unknown
	Status metav1.ConditionStatus `json:"status"`
	// Timeout is how long the condition has to hold.
	Timeout metav1.Duration `json:"timeout"`
}

// Remediation says what is done about an unhealthy router.
type Remediation struct {
	// RebootAttempts is how often the instance is rebooted before the router
	// is replaced. 0 replaces straight away.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=1
	// +optional
	RebootAttempts *int32 `json:"rebootAttempts,omitempty"`
	// RebootTimeout is how long a rebooted router gets to become healthy
	// before the next step.
	// +kubebuilder:default="5m"
	// +optional
	RebootTimeout *metav1.Duration `json:"rebootTimeout,omitempty"`
}

// RouterHealthCheckSpec says which routers are watched and when they count as
// broken.
type RouterHealthCheckSpec struct {
	// Selector matches the routers to watch.
	Selector metav1.LabelSelector `json:"selector"`
	// UnhealthyConditions each make a router unhealthy. Defaults to Ready
	// being False or Unknown for five minutes.
	// +listType=atomic
	// +optional
	UnhealthyConditions []UnhealthyCondition `json:"unhealthyConditions,omitempty"`
	// StartupTimeout is how long a new router gets to become ready for the
	// first time before it counts as unhealthy.
	// +kubebuilder:default="15m"
	// +optional
	StartupTimeout *metav1.Duration `json:"startupTimeout,omitempty"`
	// MaxUnhealthy is the number or percentage of selected routers that may
	// be unhealthy at once for remediation to still happen. Above it nothing
	// is touched: when every router looks broken, the likelier explanation is
	// that the observer is.
	// +kubebuilder:default="50%"
	// +kubebuilder:validation:XIntOrString
	// +optional
	MaxUnhealthy *intstr.IntOrString `json:"maxUnhealthy,omitempty"`
	// Remediation steps.
	// +optional
	Remediation Remediation `json:"remediation,omitempty"`
}

// RouterHealthCheckStatus is the observed state of a RouterHealthCheck.
type RouterHealthCheckStatus struct {
	// ExpectedRouters is the number of routers the selector matches.
	// +optional
	ExpectedRouters int32 `json:"expectedRouters"`
	// CurrentHealthy is the number of them that are healthy.
	// +optional
	CurrentHealthy int32 `json:"currentHealthy"`
	// RemediationsAllowed is how many more routers may become unhealthy
	// before remediation stops.
	// +optional
	RemediationsAllowed int32 `json:"remediationsAllowed"`
	// Targets are the names of the selected routers.
	// +listType=set
	// +optional
	Targets []string `json:"targets,omitempty"`
	// ObservedGeneration is the generation this status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Conditions: RemediationAllowed.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// RouterHealthCheck reboots, and failing that replaces, routers that stop
// working.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:categories=router-api,shortName=rhc
// +kubebuilder:printcolumn:name="Expected",type=integer,JSONPath=`.status.expectedRouters`
// +kubebuilder:printcolumn:name="Healthy",type=integer,JSONPath=`.status.currentHealthy`
// +kubebuilder:printcolumn:name="MaxUnhealthy",type=string,JSONPath=`.spec.maxUnhealthy`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type RouterHealthCheck struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RouterHealthCheckSpec   `json:"spec"`
	Status RouterHealthCheckStatus `json:"status,omitempty"`
}

// RouterHealthCheckList is a list of RouterHealthChecks.
// +kubebuilder:object:root=true
type RouterHealthCheckList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RouterHealthCheck `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RouterHealthCheck{}, &RouterHealthCheckList{})
}
