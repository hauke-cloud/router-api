package controller

import (
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	corev1alpha1 "github.com/hauke-cloud/router-api/api/core/v1alpha1"
	infrav1alpha1 "github.com/hauke-cloud/router-api/api/infrastructure/v1alpha1"
	"github.com/hauke-cloud/router-api/internal/conditions"
	"github.com/hauke-cloud/router-api/internal/infrastructure/hetzner/cloud"
)

func (f *fixture) serverName() string {
	return ServerName(f.hetznerMachine())
}

func readyReason(machine *infrav1alpha1.HetznerMachine) string {
	ready := conditions.Get(machine.Status.Conditions, corev1alpha1.ReadyCondition)
	if ready == nil {
		return ""
	}
	return string(ready.Status) + "/" + ready.Reason
}

func TestMachineWaitsForBootstrapData(t *testing.T) {
	f := newFixture(t)
	f.reconcileNetwork()
	f.createMachine(false)

	f.reconcileMachine()

	if len(f.cloud.Servers()) != 0 {
		t.Fatal("a server was created without user data: it would boot unreachable and unconfigured")
	}
	if got := readyReason(f.hetznerMachine()); got != "False/"+ReasonWaitingForBootstrapData {
		t.Errorf("Ready = %s", got)
	}
}

func TestMachineWaitsForTheNetwork(t *testing.T) {
	f := newFixture(t)
	f.createMachine(true)

	f.reconcileMachine()

	if len(f.cloud.Servers()) != 0 {
		t.Fatal("a server was created before its firewall exists")
	}
	if got := readyReason(f.hetznerMachine()); got != "False/"+ReasonNetworkNotReady {
		t.Errorf("Ready = %s", got)
	}
}

func TestMachineCreatesAServer(t *testing.T) {
	f := newFixture(t)
	f.reconcileNetwork()
	f.createMachine(true)

	f.reconcileMachine()

	name := f.serverName()
	spec, ok := f.cloud.Spec(name)
	if !ok {
		t.Fatalf("no server named %s; servers = %v", name, f.cloud.Servers())
	}
	network := f.net()
	if spec.ServerType != "cx23" || spec.Location != "fsn1" || spec.Image != "ubuntu-24.04" {
		t.Errorf("spec = %+v", spec)
	}
	if spec.UserData != "#cloud-config\n" {
		t.Errorf("user data = %q", spec.UserData)
	}
	if spec.NetworkID != network.Status.NetworkID || spec.FirewallID != network.Status.FirewallID ||
		spec.PlacementGroupID != network.Status.PlacementGroupID {
		t.Errorf("spec = %+v, network status = %+v", spec, network.Status)
	}
	if !spec.EnableIPv4 || !spec.EnableIPv6 {
		t.Errorf("public net = %v/%v", spec.EnableIPv4, spec.EnableIPv6)
	}
	if len(spec.SSHKeys) != 1 || spec.SSHKeys[0] != "yubikey" {
		t.Errorf("ssh keys = %v", spec.SSHKeys)
	}
	machine := f.hetznerMachine()
	if spec.Labels[MachineUIDLabel] != string(machine.UID) {
		t.Errorf("labels = %v: nothing ties the server to this object", spec.Labels)
	}

	// Created, still initializing.
	if machine.Spec.ProviderID == "" {
		t.Error("no providerID")
	}
	if !controllerutil.ContainsFinalizer(machine, infrav1alpha1.HetznerMachineFinalizer) {
		t.Error("no finalizer")
	}
	if machine.Status.Initialization != nil && ptr.Deref(machine.Status.Initialization.Provisioned, false) {
		t.Error("provisioned while the server is still initializing")
	}
	if got := readyReason(machine); got != "False/"+ReasonServerNotRunning {
		t.Errorf("Ready = %s", got)
	}

	f.cloud.SetServerStatus(name, cloud.ServerStatusRunning)
	f.reconcileMachine()
	f.reconcileMachine()

	machine = f.hetznerMachine()
	if machine.Status.Initialization == nil || !ptr.Deref(machine.Status.Initialization.Provisioned, false) {
		t.Error("not provisioned")
	}
	if got := readyReason(machine); got != "True/"+ReasonServerRunning {
		t.Errorf("Ready = %s", got)
	}
	types := map[corev1alpha1.AddressType]int{}
	for _, address := range machine.Status.Addresses {
		types[address.Type]++
	}
	if types[corev1alpha1.AddressExternalIP] != 2 || types[corev1alpha1.AddressInternalIP] != 1 {
		t.Errorf("addresses = %v", machine.Status.Addresses)
	}
	if machine.Status.FailureDomain != "fsn1" || machine.Status.ServerStatus != cloud.ServerStatusRunning {
		t.Errorf("status = %+v", machine.Status)
	}
	if got := len(f.cloud.Servers()); got != 1 {
		t.Errorf("%d servers after three reconciles", got)
	}
}

func TestMachineDoesNotRecreateALostServer(t *testing.T) {
	f := newFixture(t)
	f.reconcileNetwork()
	f.createMachine(true)
	f.reconcileMachine()
	name := f.serverName()
	f.cloud.SetServerStatus(name, cloud.ServerStatusRunning)
	f.reconcileMachine()

	f.cloud.RemoveServer(name)
	f.reconcileMachine()

	// A new server from the old user data would come up claiming to be a
	// router that the config provider believes it already configured. A
	// lost server is reported, and replacing the router is someone else's
	// decision.
	if got := len(f.cloud.Servers()); got != 0 {
		t.Fatalf("%d servers: the lost server was recreated", got)
	}
	if got := readyReason(f.hetznerMachine()); got != "False/"+ReasonServerNotFound {
		t.Errorf("Ready = %s", got)
	}
}

func TestMachineRefusesAServerThatIsNotItsOwn(t *testing.T) {
	f := newFixture(t)
	f.reconcileNetwork()
	f.createMachine(true)
	f.cloud.AddServer(f.serverName(), map[string]string{MachineUIDLabel: "someone-else"})

	f.reconcileMachine()

	if got := readyReason(f.hetznerMachine()); got != "False/"+ReasonNameConflict {
		t.Errorf("Ready = %s", got)
	}
	if f.hetznerMachine().Spec.ProviderID != "" {
		t.Error("adopted a server that belongs to something else")
	}
}

func TestMachineReboots(t *testing.T) {
	f := newFixture(t)
	f.reconcileNetwork()
	f.createMachine(true)
	f.reconcileMachine()
	name := f.serverName()
	f.cloud.SetServerStatus(name, cloud.ServerStatusRunning)
	f.reconcileMachine()

	machine := f.hetznerMachine()
	machine.Annotations = map[string]string{corev1alpha1.RemediationAnnotation: "Reboot/1/2026-10-08T12:00:00Z"}
	if err := k8s.Update(f.ctx, machine); err != nil {
		t.Fatal(err)
	}
	f.reconcileMachine()
	f.reconcileMachine()

	if got := f.cloud.Resets(name); got != 1 {
		t.Errorf("%d resets, want exactly 1 for one request", got)
	}
	if got := f.hetznerMachine().Status.LastRemediation; got != "Reboot/1/2026-10-08T12:00:00Z" {
		t.Errorf("lastRemediation = %q", got)
	}

	machine = f.hetznerMachine()
	machine.Annotations[corev1alpha1.RemediationAnnotation] = "Reboot/2/2026-10-08T12:10:00Z"
	if err := k8s.Update(f.ctx, machine); err != nil {
		t.Fatal(err)
	}
	f.reconcileMachine()
	if got := f.cloud.Resets(name); got != 2 {
		t.Errorf("%d resets after a second request", got)
	}
}

func TestMachineDeletion(t *testing.T) {
	f := newFixture(t)
	f.reconcileNetwork()
	f.createMachine(true)
	f.reconcileMachine()

	if err := k8s.Delete(f.ctx, f.hetznerMachine()); err != nil {
		t.Fatal(err)
	}
	f.reconcileMachine()

	if got := len(f.cloud.Servers()); got != 0 {
		t.Errorf("%d servers left", got)
	}
	if err := k8s.Get(f.ctx, f.key("edge"), &infrav1alpha1.HetznerMachine{}); !apierrors.IsNotFound(err) {
		t.Errorf("the machine object still exists: %v", err)
	}
}

func TestMachineDeletionWithoutANetwork(t *testing.T) {
	f := newFixture(t)
	f.reconcileNetwork()
	f.createMachine(true)
	f.reconcileMachine()

	// The network object is forced away first.
	network := f.net()
	network.Finalizers = nil
	if err := k8s.Update(f.ctx, network); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Delete(f.ctx, network); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Delete(f.ctx, f.hetznerMachine()); err != nil {
		t.Fatal(err)
	}
	f.reconcileMachine()

	// Without the token there is no way to delete the server. Dropping the
	// finalizer anyway would leave it running and billed with nothing in
	// the cluster knowing about it.
	machine := f.hetznerMachine()
	if !controllerutil.ContainsFinalizer(machine, infrav1alpha1.HetznerMachineFinalizer) {
		t.Fatal("the finalizer was removed although the server could not be deleted")
	}
	ready := conditions.Get(machine.Status.Conditions, corev1alpha1.ReadyCondition)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != ReasonNetworkNotReady {
		t.Errorf("Ready = %+v", ready)
	}
}
