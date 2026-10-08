package contract

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/yaml"

	corev1alpha1 "github.com/hauke-cloud/router-api/api/core/v1alpha1"
)

func parse(t *testing.T, manifest string) *unstructured.Unstructured {
	t.Helper()
	object := &unstructured.Unstructured{}
	if err := yaml.Unmarshal([]byte(manifest), &object.Object); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	return object
}

const infraMachine = `
apiVersion: infrastructure.router.hauke.cloud/v1alpha1
kind: HetznerMachine
metadata:
  name: edge-abc
  namespace: routers
  generation: 3
spec:
  providerID: hcloud://4711
status:
  initialization:
    provisioned: true
  failureDomain: fsn1
  lastRemediation: Reboot/2
  addresses:
    - type: ExternalIP
      address: 203.0.113.7
    - type: InternalIP
      address: 10.0.1.2
    - type: Bogus
  conditions:
    - type: Ready
      status: "True"
      reason: ServerRunning
      message: up
      observedGeneration: 3
      lastTransitionTime: "2026-10-08T10:00:00Z"
`

func TestInfrastructureAccessors(t *testing.T) {
	object := parse(t, infraMachine)

	if !InfrastructureProvisioned(object) {
		t.Error("InfrastructureProvisioned = false")
	}
	if got := ProviderID(object); got != "hcloud://4711" {
		t.Errorf("ProviderID = %q", got)
	}
	if got := FailureDomain(object); got != "fsn1" {
		t.Errorf("FailureDomain = %q", got)
	}
	if got := LastRemediation(object); got != "Reboot/2" {
		t.Errorf("LastRemediation = %q", got)
	}

	// An entry a provider got wrong is dropped, not passed on half filled.
	want := []corev1alpha1.MachineAddress{
		{Type: corev1alpha1.AddressExternalIP, Address: "203.0.113.7"},
		{Type: corev1alpha1.AddressInternalIP, Address: "10.0.1.2"},
	}
	got := Addresses(object)
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("Addresses = %v, want %v", got, want)
	}
}

func TestAccessorsOnAnEmptyObject(t *testing.T) {
	object := parse(t, "apiVersion: x/v1\nkind: Thing\nmetadata: {name: a}\n")

	if InfrastructureProvisioned(object) || BootstrapDataSecretCreated(object) {
		t.Error("an object without status reports itself initialised")
	}
	if ProviderID(object) != "" || BootstrapDataSecretName(object) != "" || ReplacementHash(object) != "" {
		t.Error("an object without the fields reports values")
	}
	if Addresses(object) != nil {
		t.Error("Addresses != nil")
	}
	if Condition(object, "Ready") != nil {
		t.Error("Condition != nil")
	}
	if IsConditionTrue(object, "Ready") {
		t.Error("IsConditionTrue = true")
	}
}

func TestCondition(t *testing.T) {
	object := parse(t, infraMachine)

	condition := Condition(object, corev1alpha1.ReadyCondition)
	if condition == nil {
		t.Fatal("Condition = nil")
	}
	if condition.Status != metav1.ConditionTrue || condition.Reason != "ServerRunning" ||
		condition.Message != "up" || condition.ObservedGeneration != 3 || condition.LastTransitionTime.IsZero() {
		t.Errorf("condition = %+v", condition)
	}
	if !IsConditionTrue(object, corev1alpha1.ReadyCondition) {
		t.Error("IsConditionTrue = false")
	}
	if Condition(object, "Other") != nil {
		t.Error("an absent condition was found")
	}
}

func TestStaleConditionIsNotTrue(t *testing.T) {
	object := parse(t, infraMachine)
	object.SetGeneration(4)

	// The provider has not looked at generation 4 yet, so its verdict is
	// about a spec that no longer exists.
	if IsConditionTrue(object, corev1alpha1.ReadyCondition) {
		t.Error("a condition observed at an older generation counts as true")
	}
}

func TestBootstrapAccessors(t *testing.T) {
	object := parse(t, `
apiVersion: config.router.hauke.cloud/v1alpha1
kind: VyOSConfig
metadata: {name: edge-abc}
status:
  dataSecretName: edge-abc-bootstrap
  version: 2026.10.07-0712-rolling
  initialization:
    dataSecretCreated: true
`)
	if got := BootstrapDataSecretName(object); got != "edge-abc-bootstrap" {
		t.Errorf("BootstrapDataSecretName = %q", got)
	}
	if !BootstrapDataSecretCreated(object) {
		t.Error("BootstrapDataSecretCreated = false")
	}
	if got := Version(object); got != "2026.10.07-0712-rolling" {
		t.Errorf("Version = %q", got)
	}
}

const configTemplate = `
apiVersion: config.router.hauke.cloud/v1alpha1
kind: VyOSConfigTemplate
metadata:
  name: edge
  namespace: routers
  uid: 1234
  resourceVersion: "9"
spec:
  template:
    metadata:
      labels: {tier: edge}
      annotations: {note: hi}
    spec:
      image: ghcr.io/hauke-cloud/vyos@sha256:abc
      commands: set system host-name edge
status:
  replacementHash: f00
`

func TestReplacementHash(t *testing.T) {
	if got := ReplacementHash(parse(t, configTemplate)); got != "f00" {
		t.Errorf("ReplacementHash = %q", got)
	}
}

func TestFromTemplate(t *testing.T) {
	template := parse(t, configTemplate)
	owner := &metav1.OwnerReference{APIVersion: "router.hauke.cloud/v1alpha1", Kind: "Router", Name: "edge-abc", UID: types.UID("u1")}

	object, err := FromTemplate(template, "edge-abc", map[string]string{"router.hauke.cloud/router-name": "edge-abc", "tier": "ignored"}, owner)
	if err != nil {
		t.Fatalf("FromTemplate: %v", err)
	}

	if object.GetAPIVersion() != "config.router.hauke.cloud/v1alpha1" || object.GetKind() != "VyOSConfig" {
		t.Errorf("gvk = %s %s", object.GetAPIVersion(), object.GetKind())
	}
	if object.GetName() != "edge-abc" || object.GetNamespace() != "routers" {
		t.Errorf("name = %s/%s", object.GetNamespace(), object.GetName())
	}
	if object.GetUID() != "" || object.GetResourceVersion() != "" {
		t.Error("server-set metadata was copied from the template")
	}
	// The template's own labels win: the caller's are bookkeeping.
	labels := object.GetLabels()
	if labels["tier"] != "edge" || labels["router.hauke.cloud/router-name"] != "edge-abc" {
		t.Errorf("labels = %v", labels)
	}
	annotations := object.GetAnnotations()
	if annotations["note"] != "hi" ||
		annotations[corev1alpha1.ClonedFromNameAnnotation] != "edge" ||
		annotations[corev1alpha1.ClonedFromGroupKindAnnotation] != "VyOSConfigTemplate.config.router.hauke.cloud" {
		t.Errorf("annotations = %v", annotations)
	}
	if refs := object.GetOwnerReferences(); len(refs) != 1 || refs[0].Name != "edge-abc" {
		t.Errorf("ownerReferences = %v", refs)
	}
	image, _, _ := unstructured.NestedString(object.Object, "spec", "image")
	if image != "ghcr.io/hauke-cloud/vyos@sha256:abc" {
		t.Errorf("spec.image = %q", image)
	}
	if _, found, _ := unstructured.NestedMap(object.Object, "status"); found {
		t.Error("status was copied from the template")
	}

	// The clone must not share nested maps with the template.
	if err := unstructured.SetNestedField(object.Object, "changed", "spec", "image"); err != nil {
		t.Fatal(err)
	}
	original, _, _ := unstructured.NestedString(template.Object, "spec", "template", "spec", "image")
	if original != "ghcr.io/hauke-cloud/vyos@sha256:abc" {
		t.Error("changing the clone changed the template")
	}
}

func TestFromTemplateRejectsNonTemplates(t *testing.T) {
	if _, err := FromTemplate(parse(t, infraMachine), "x", nil, nil); err == nil {
		t.Error("an object whose kind does not end in Template was cloned")
	}
	noSpec := parse(t, "apiVersion: x/v1\nkind: ThingTemplate\nmetadata: {name: a}\n")
	if _, err := FromTemplate(noSpec, "x", nil, nil); err == nil {
		t.Error("a template without spec.template.spec was cloned")
	}
}

func TestTemplateSpec(t *testing.T) {
	spec, err := TemplateSpec(parse(t, configTemplate))
	if err != nil {
		t.Fatalf("TemplateSpec: %v", err)
	}
	if spec["commands"] != "set system host-name edge" {
		t.Errorf("spec = %v", spec)
	}
}
