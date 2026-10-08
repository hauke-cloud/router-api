package routermachine

import (
	"context"
	"os"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1alpha1 "github.com/hauke-cloud/router-api/api/core/v1alpha1"
	infrav1alpha1 "github.com/hauke-cloud/router-api/api/infrastructure/v1alpha1"
	"github.com/hauke-cloud/router-api/internal/conditions"
	"github.com/hauke-cloud/router-api/internal/testenv"
)

var k8s client.Client

func TestMain(m *testing.M) {
	os.Exit(testenv.Run(m, func(c client.Client) { k8s = c }))
}

type fixture struct {
	t         *testing.T
	ctx       context.Context
	namespace string
	r         *Reconciler
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	return &fixture{t: t, ctx: context.Background(), namespace: testenv.Namespace(t, k8s), r: &Reconciler{Client: k8s}}
}

// create makes a RouterMachine named edge and the HetznerMachine it refers to.
func (f *fixture) create() {
	f.t.Helper()
	infra := &infrav1alpha1.HetznerMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: f.namespace},
		Spec: infrav1alpha1.HetznerMachineSpec{
			NetworkRef: corev1alpha1.LocalObjectReference{Name: "net"}, ServerType: "cx23", Location: "fsn1",
		},
	}
	if err := k8s.Create(f.ctx, infra); err != nil {
		f.t.Fatalf("create HetznerMachine: %v", err)
	}
	machine := &corev1alpha1.RouterMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: f.namespace},
		Spec: corev1alpha1.RouterMachineSpec{
			InfrastructureRef: corev1alpha1.ContractReference{
				APIGroup: infrav1alpha1.GroupVersion.Group, Kind: "HetznerMachine", Name: "edge",
			},
		},
	}
	if err := k8s.Create(f.ctx, machine); err != nil {
		f.t.Fatalf("create RouterMachine: %v", err)
	}
}

func (f *fixture) key() types.NamespacedName {
	return types.NamespacedName{Namespace: f.namespace, Name: "edge"}
}

func (f *fixture) reconcile() reconcile.Result {
	f.t.Helper()
	result, err := f.r.Reconcile(f.ctx, reconcile.Request{NamespacedName: f.key()})
	if err != nil {
		f.t.Fatalf("Reconcile: %v", err)
	}
	return result
}

func (f *fixture) machine() *corev1alpha1.RouterMachine {
	f.t.Helper()
	machine := &corev1alpha1.RouterMachine{}
	if err := k8s.Get(f.ctx, f.key(), machine); err != nil {
		f.t.Fatalf("get RouterMachine: %v", err)
	}
	return machine
}

func (f *fixture) infra() *infrav1alpha1.HetznerMachine {
	f.t.Helper()
	infra := &infrav1alpha1.HetznerMachine{}
	if err := k8s.Get(f.ctx, f.key(), infra); err != nil {
		f.t.Fatalf("get HetznerMachine: %v", err)
	}
	return infra
}

// provision plays the infrastructure provider reporting a running server.
func (f *fixture) provision() {
	f.t.Helper()
	infra := f.infra()
	infra.Spec.ProviderID = "hcloud://4711"
	if err := k8s.Update(f.ctx, infra); err != nil {
		f.t.Fatalf("update HetznerMachine: %v", err)
	}
	infra.Status.Initialization = &infrav1alpha1.HetznerMachineInitialization{Provisioned: ptr.To(true)}
	infra.Status.FailureDomain = "fsn1"
	infra.Status.Addresses = []corev1alpha1.MachineAddress{
		{Type: corev1alpha1.AddressExternalIP, Address: "203.0.113.7"},
		{Type: corev1alpha1.AddressInternalIP, Address: "10.0.1.2"},
	}
	conditions.True(&infra.Status.Conditions, infra.Generation, corev1alpha1.ReadyCondition, "ServerRunning", "")
	if err := k8s.Status().Update(f.ctx, infra); err != nil {
		f.t.Fatalf("update HetznerMachine status: %v", err)
	}
}

func TestWaitsForInfrastructure(t *testing.T) {
	f := newFixture(t)
	f.create()

	f.reconcile()

	machine := f.machine()
	if !controllerutil.ContainsFinalizer(machine, corev1alpha1.RouterMachineFinalizer) {
		t.Error("no finalizer: the machine could be deleted and leave its server behind")
	}
	if machine.Status.Phase != corev1alpha1.RouterMachinePhasePending {
		t.Errorf("phase = %q, want Pending while there is no bootstrap data", machine.Status.Phase)
	}
	if conditions.IsTrue(machine.Status.Conditions, corev1alpha1.ReadyCondition) {
		t.Error("Ready is True before the infrastructure is")
	}

	// The infrastructure object goes when the machine goes.
	owners := f.infra().OwnerReferences
	if len(owners) != 1 || owners[0].Kind != "RouterMachine" || owners[0].Name != "edge" || !ptr.Deref(owners[0].Controller, false) {
		t.Errorf("ownerReferences = %+v", owners)
	}
}

func TestProvisioningPhase(t *testing.T) {
	f := newFixture(t)
	f.create()
	machine := f.machine()
	machine.Spec.Bootstrap.DataSecretName = ptr.To("edge-bootstrap")
	if err := k8s.Update(f.ctx, machine); err != nil {
		t.Fatal(err)
	}

	f.reconcile()

	if got := f.machine().Status.Phase; got != corev1alpha1.RouterMachinePhaseProvisioning {
		t.Errorf("phase = %q, want Provisioning", got)
	}
}

func TestMirrorsInfrastructure(t *testing.T) {
	f := newFixture(t)
	f.create()
	f.reconcile()
	f.provision()

	f.reconcile()

	machine := f.machine()
	if machine.Spec.ProviderID != "hcloud://4711" {
		t.Errorf("providerID = %q", machine.Spec.ProviderID)
	}
	if !machine.Status.InfrastructureProvisioned {
		t.Error("InfrastructureProvisioned = false")
	}
	if machine.Status.FailureDomain != "fsn1" || len(machine.Status.Addresses) != 2 {
		t.Errorf("status = %+v", machine.Status)
	}
	if machine.Status.Phase != corev1alpha1.RouterMachinePhaseRunning {
		t.Errorf("phase = %q", machine.Status.Phase)
	}
	for _, conditionType := range []string{corev1alpha1.ReadyCondition, corev1alpha1.InfrastructureReadyCondition} {
		if !conditions.IsTrue(machine.Status.Conditions, conditionType) {
			t.Errorf("%s is not True", conditionType)
		}
	}
	if machine.Status.ObservedGeneration != machine.Generation {
		t.Errorf("observedGeneration = %d, generation = %d", machine.Status.ObservedGeneration, machine.Generation)
	}
}

func TestInfrastructureGoesUnready(t *testing.T) {
	f := newFixture(t)
	f.create()
	f.reconcile()
	f.provision()
	f.reconcile()

	infra := f.infra()
	conditions.False(&infra.Status.Conditions, infra.Generation, corev1alpha1.ReadyCondition, "ServerOff", "the server is off")
	if err := k8s.Status().Update(f.ctx, infra); err != nil {
		t.Fatal(err)
	}
	f.reconcile()

	machine := f.machine()
	ready := conditions.Get(machine.Status.Conditions, corev1alpha1.ReadyCondition)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != "ServerOff" {
		t.Errorf("Ready = %+v", ready)
	}
	// Provisioned records that the server was created. It is not undone by
	// the server being off: nothing may conclude it has to be created again.
	if !machine.Status.InfrastructureProvisioned {
		t.Error("InfrastructureProvisioned went back to false")
	}
}

func TestMissingInfrastructure(t *testing.T) {
	f := newFixture(t)
	machine := &corev1alpha1.RouterMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: f.namespace},
		Spec: corev1alpha1.RouterMachineSpec{
			InfrastructureRef: corev1alpha1.ContractReference{
				APIGroup: infrav1alpha1.GroupVersion.Group, Kind: "HetznerMachine", Name: "edge",
			},
		},
	}
	if err := k8s.Create(f.ctx, machine); err != nil {
		t.Fatal(err)
	}

	result := f.reconcile()

	if result.RequeueAfter == 0 {
		t.Error("no requeue: nothing would notice the object appearing")
	}
	condition := conditions.Get(f.machine().Status.Conditions, corev1alpha1.InfrastructureReadyCondition)
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != ReasonInfrastructureNotFound {
		t.Errorf("InfrastructureReady = %+v", condition)
	}
}

func TestRemediationRequestIsPassedOn(t *testing.T) {
	f := newFixture(t)
	f.create()
	f.reconcile()

	machine := f.machine()
	machine.Annotations = map[string]string{corev1alpha1.RemediationAnnotation: "Reboot/1"}
	if err := k8s.Update(f.ctx, machine); err != nil {
		t.Fatal(err)
	}
	f.reconcile()

	infra := f.infra()
	if got := infra.Annotations[corev1alpha1.RemediationAnnotation]; got != "Reboot/1" {
		t.Fatalf("annotation on the infrastructure object = %q", got)
	}

	// The provider answers by recording what it acted on.
	infra.Status.LastRemediation = "Reboot/1"
	if err := k8s.Status().Update(f.ctx, infra); err != nil {
		t.Fatal(err)
	}
	f.reconcile()
	if got := f.machine().Status.LastRemediation; got != "Reboot/1" {
		t.Errorf("lastRemediation = %q", got)
	}
}

func TestPaused(t *testing.T) {
	f := newFixture(t)
	f.create()
	machine := f.machine()
	machine.Annotations = map[string]string{corev1alpha1.PausedAnnotation: "true"}
	if err := k8s.Update(f.ctx, machine); err != nil {
		t.Fatal(err)
	}

	f.reconcile()

	machine = f.machine()
	if !conditions.IsTrue(machine.Status.Conditions, corev1alpha1.PausedCondition) {
		t.Error("Paused is not True")
	}
	if len(f.infra().OwnerReferences) != 0 {
		t.Error("a paused machine still wrote to its infrastructure object")
	}
}

func TestDeletion(t *testing.T) {
	f := newFixture(t)
	f.create()
	f.reconcile()

	// Stand in for the provider's finalizer, so the infrastructure object
	// lingers the way it does while a server is being deleted.
	infra := f.infra()
	controllerutil.AddFinalizer(infra, infrav1alpha1.HetznerMachineFinalizer)
	if err := k8s.Update(f.ctx, infra); err != nil {
		t.Fatal(err)
	}

	if err := k8s.Delete(f.ctx, f.machine()); err != nil {
		t.Fatal(err)
	}
	result := f.reconcile()

	if f.infra().DeletionTimestamp.IsZero() {
		t.Fatal("the infrastructure object was not deleted")
	}
	machine := f.machine()
	if !controllerutil.ContainsFinalizer(machine, corev1alpha1.RouterMachineFinalizer) {
		t.Fatal("the finalizer was removed while the server still exists")
	}
	if machine.Status.Phase != corev1alpha1.RouterMachinePhaseDeleting {
		t.Errorf("phase = %q", machine.Status.Phase)
	}
	if result.RequeueAfter == 0 {
		t.Error("no requeue while waiting for the server to go")
	}

	// The provider finishes.
	infra = f.infra()
	controllerutil.RemoveFinalizer(infra, infrav1alpha1.HetznerMachineFinalizer)
	if err := k8s.Update(f.ctx, infra); err != nil {
		t.Fatal(err)
	}
	f.reconcile()

	err := k8s.Get(f.ctx, f.key(), &corev1alpha1.RouterMachine{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("the machine still exists: %v", err)
	}
}

func TestReconcileOfAGoneMachine(t *testing.T) {
	f := newFixture(t)
	if result := f.reconcile(); result.RequeueAfter != 0 {
		t.Errorf("result = %+v", result)
	}
}
