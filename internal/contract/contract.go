// Package contract is the only way core controllers touch provider objects.
//
// Core never imports a provider's Go types. It reads and writes the handful of
// fields the provider contract names (docs/provider-contract.md) on
// unstructured objects, so that a new infrastructure or config provider is a
// new set of CRDs and a new binary, and nothing in core changes.
package contract

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/hauke-cloud/router-api/api/core/v1alpha1"
)

// TemplateSuffix is what a template's kind ends in. The kind it stamps out is
// the same name without it.
const TemplateSuffix = "Template"

// BootstrapDataKey is the key of a bootstrap Secret that holds the user data.
const BootstrapDataKey = "value"

// GroupVersionKind resolves a reference to the version the API server
// prefers. References carry no version on purpose: a provider can then
// introduce a new one without anyone having to rewrite their manifests.
func GroupVersionKind(mapper meta.RESTMapper, ref corev1alpha1.ContractReference) (schema.GroupVersionKind, error) {
	mapping, err := mapper.RESTMapping(schema.GroupKind{Group: ref.APIGroup, Kind: ref.Kind})
	if err != nil {
		return schema.GroupVersionKind{}, fmt.Errorf("resolve %s.%s: %w", ref.Kind, ref.APIGroup, err)
	}
	return mapping.GroupVersionKind, nil
}

// Get fetches the object a reference points at.
func Get(ctx context.Context, c client.Client, namespace string, ref corev1alpha1.ContractReference) (*unstructured.Unstructured, error) {
	gvk, err := GroupVersionKind(c.RESTMapper(), ref)
	if err != nil {
		return nil, err
	}
	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(gvk)
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: ref.Name}, object); err != nil {
		return nil, err
	}
	return object, nil
}

// Reference returns the reference that points at object.
func Reference(object *unstructured.Unstructured) corev1alpha1.ContractReference {
	return corev1alpha1.ContractReference{
		APIGroup: object.GroupVersionKind().Group,
		Kind:     object.GetKind(),
		Name:     object.GetName(),
	}
}

// --- infrastructure machine -------------------------------------------------

// InfrastructureProvisioned reports status.initialization.provisioned.
func InfrastructureProvisioned(object *unstructured.Unstructured) bool {
	provisioned, _, _ := unstructured.NestedBool(object.Object, "status", "initialization", "provisioned")
	return provisioned
}

// ProviderID returns spec.providerID.
func ProviderID(object *unstructured.Unstructured) string {
	return nestedString(object, "spec", "providerID")
}

// FailureDomain returns status.failureDomain.
func FailureDomain(object *unstructured.Unstructured) string {
	return nestedString(object, "status", "failureDomain")
}

// LastRemediation returns status.lastRemediation: the value of the
// remediation annotation the provider acted on last.
func LastRemediation(object *unstructured.Unstructured) string {
	return nestedString(object, "status", "lastRemediation")
}

// Addresses returns status.addresses. Entries without a known type or without
// an address are dropped.
func Addresses(object *unstructured.Unstructured) []corev1alpha1.MachineAddress {
	raw, _, _ := unstructured.NestedSlice(object.Object, "status", "addresses")
	var addresses []corev1alpha1.MachineAddress
	for _, item := range raw {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		addressType, _ := entry["type"].(string)
		address, _ := entry["address"].(string)
		if address == "" || !knownAddressType(corev1alpha1.AddressType(addressType)) {
			continue
		}
		addresses = append(addresses, corev1alpha1.MachineAddress{
			Type:    corev1alpha1.AddressType(addressType),
			Address: address,
		})
	}
	return addresses
}

func knownAddressType(t corev1alpha1.AddressType) bool {
	switch t {
	case corev1alpha1.AddressHostname, corev1alpha1.AddressExternalIP, corev1alpha1.AddressInternalIP,
		corev1alpha1.AddressExternalDNS, corev1alpha1.AddressInternalDNS:
		return true
	}
	return false
}

// --- config -----------------------------------------------------------------

// BootstrapDataSecretName returns status.dataSecretName.
func BootstrapDataSecretName(object *unstructured.Unstructured) string {
	return nestedString(object, "status", "dataSecretName")
}

// BootstrapDataSecretCreated reports status.initialization.dataSecretCreated.
func BootstrapDataSecretCreated(object *unstructured.Unstructured) bool {
	created, _, _ := unstructured.NestedBool(object.Object, "status", "initialization", "dataSecretCreated")
	return created
}

// Version returns status.version: the software version the router reports.
func Version(object *unstructured.Unstructured) string {
	return nestedString(object, "status", "version")
}

// ReplacementHash returns status.replacementHash of a config template. It
// covers the fields a running router cannot change; core replaces routers
// when it changes.
func ReplacementHash(object *unstructured.Unstructured) string {
	return nestedString(object, "status", "replacementHash")
}

// --- conditions -------------------------------------------------------------

// Condition returns the condition of the given type, or nil.
func Condition(object *unstructured.Unstructured, conditionType string) *metav1.Condition {
	raw, _, _ := unstructured.NestedSlice(object.Object, "status", "conditions")
	for _, item := range raw {
		entry, ok := item.(map[string]any)
		if !ok || entry["type"] != conditionType {
			continue
		}
		condition := &metav1.Condition{Type: conditionType}
		if status, ok := entry["status"].(string); ok {
			condition.Status = metav1.ConditionStatus(status)
		}
		condition.Reason, _ = entry["reason"].(string)
		condition.Message, _ = entry["message"].(string)
		condition.ObservedGeneration = asInt64(entry["observedGeneration"])
		if stamp, ok := entry["lastTransitionTime"].(string); ok {
			if parsed, err := time.Parse(time.RFC3339, stamp); err == nil {
				condition.LastTransitionTime = metav1.NewTime(parsed)
			}
		}
		return condition
	}
	return nil
}

// IsConditionTrue reports whether the condition is True for the object's
// current generation. A verdict a provider reached about an older generation
// says nothing about the spec that is there now.
func IsConditionTrue(object *unstructured.Unstructured, conditionType string) bool {
	condition := Condition(object, conditionType)
	if condition == nil || condition.Status != metav1.ConditionTrue {
		return false
	}
	return condition.ObservedGeneration == 0 || condition.ObservedGeneration >= object.GetGeneration()
}

// ExposedField is the field of a config object's spec that core fills in with
// the endpoints of the RouterExposure the spec refers to.
const ExposedField = "exposed"

// ExposureRef returns the name of the RouterExposure a config spec refers to
// in exposureRef.name, or "".
func ExposureRef(spec map[string]any) string {
	name, _, _ := unstructured.NestedString(spec, "exposureRef", "name")
	return name
}

// --- templates --------------------------------------------------------------

// TemplateSpec returns a copy of spec.template.spec of a template.
func TemplateSpec(template *unstructured.Unstructured) (map[string]any, error) {
	spec, found, err := unstructured.NestedMap(template.Object, "spec", "template", "spec")
	if err != nil {
		return nil, fmt.Errorf("%s %s: spec.template.spec: %w", template.GetKind(), template.GetName(), err)
	}
	if !found {
		return nil, fmt.Errorf("%s %s has no spec.template.spec", template.GetKind(), template.GetName())
	}
	return runtime.DeepCopyJSON(spec), nil
}

// FromTemplate stamps an object out of a template: the kind is the template's
// without the Template suffix, the spec is spec.template.spec, and labels and
// annotations from spec.template.metadata are layered over the ones given.
func FromTemplate(template *unstructured.Unstructured, name string, labels map[string]string, owner *metav1.OwnerReference) (*unstructured.Unstructured, error) {
	kind := template.GetKind()
	if !strings.HasSuffix(kind, TemplateSuffix) || kind == TemplateSuffix {
		return nil, fmt.Errorf("%s %s is not a template: its kind does not end in %q", kind, template.GetName(), TemplateSuffix)
	}
	spec, err := TemplateSpec(template)
	if err != nil {
		return nil, err
	}

	object := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	object.SetAPIVersion(template.GetAPIVersion())
	object.SetKind(strings.TrimSuffix(kind, TemplateSuffix))
	object.SetName(name)
	object.SetNamespace(template.GetNamespace())

	templateLabels, _, _ := unstructured.NestedStringMap(template.Object, "spec", "template", "metadata", "labels")
	object.SetLabels(merge(labels, templateLabels))

	templateAnnotations, _, _ := unstructured.NestedStringMap(template.Object, "spec", "template", "metadata", "annotations")
	object.SetAnnotations(merge(templateAnnotations, map[string]string{
		corev1alpha1.ClonedFromNameAnnotation:      template.GetName(),
		corev1alpha1.ClonedFromGroupKindAnnotation: template.GroupVersionKind().GroupKind().String(),
	}))

	if owner != nil {
		object.SetOwnerReferences([]metav1.OwnerReference{*owner})
	}
	return object, nil
}

// SetSpec replaces the spec of object with a copy of spec. It reports whether
// that changed anything.
func SetSpec(object *unstructured.Unstructured, spec map[string]any) (bool, error) {
	current, _, err := unstructured.NestedMap(object.Object, "spec")
	if err != nil {
		return false, errors.New("spec is not an object")
	}
	if equalJSON(current, spec) {
		return false, nil
	}
	object.Object["spec"] = runtime.DeepCopyJSON(spec)
	return true, nil
}

func merge(maps ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func nestedString(object *unstructured.Unstructured, fields ...string) string {
	value, _, _ := unstructured.NestedString(object.Object, fields...)
	return value
}

// asInt64 reads a JSON number. The API machinery decodes integers as int64,
// other decoders hand out float64.
func asInt64(value any) int64 {
	switch n := value.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	case int:
		return int64(n)
	}
	return 0
}

// NestedInt64 reads an integer field, whatever number type it was decoded as.
func NestedInt64(object *unstructured.Unstructured, fields ...string) (value int64, found bool, err error) {
	raw, found, err := unstructured.NestedFieldNoCopy(object.Object, fields...)
	if err != nil || !found {
		return 0, found, err
	}
	return asInt64(raw), true, nil
}
