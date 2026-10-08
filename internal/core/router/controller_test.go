package router

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

	configv1alpha1 "github.com/hauke-cloud/router-api/api/config/v1alpha1"
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

// newFixture creates a Router named edge with its RouterMachine and
// VyOSConfig, the way a RouterSet does.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, ctx: context.Background(), namespace: testenv.Namespace(t, k8s), r: &Reconciler{Client: k8s}}

	objects := []client.Object{
		&configv1alpha1.VyOSConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: f.namespace},
			Spec:       configv1alpha1.VyOSConfigSpec{Image: "ghcr.io/hauke-cloud/vyos:test"},
		},
		&corev1alpha1.RouterMachine{
			ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: f.namespace},
			Spec: corev1alpha1.RouterMachineSpec{InfrastructureRef: corev1alpha1.ContractReference{
				APIGroup: infrav1alpha1.GroupVersion.Group, Kind: "HetznerMachine", Name: "edge",
			}},
		},
		&corev1alpha1.Router{
			ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: f.namespace},
			Spec: corev1alpha1.RouterSpec{
				MachineRef: corev1alpha1.LocalObjectReference{Name: "edge"},
				ConfigRef: corev1alpha1.ContractReference{
					APIGroup: configv1alpha1.GroupVersion.Group, Kind: "VyOSConfig", Name: "edge",
				},
			},
		},
	}
	for _, object := range objects {
		if err := k8s.Create(f.ctx, object); err != nil {
			t.Fatalf("create %T: %v", object, err)
		}
	}
	return f
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

func get[T client.Object](f *fixture, object T) T {
	f.t.Helper()
	if err := k8s.Get(f.ctx, f.key(), object); err != nil {
		f.t.Fatalf("get %T: %v", object, err)
	}
	return object
}

func (f *fixture) router() *corev1alpha1.Router         { return get(f, &corev1alpha1.Router{}) }
func (f *fixture) machine() *corev1alpha1.RouterMachine { return get(f, &corev1alpha1.RouterMachine{}) }
func (f *fixture) config() *configv1alpha1.VyOSConfig   { return get(f, &configv1alpha1.VyOSConfig{}) }

// publishBootstrap plays the config provider publishing the user data.
func (f *fixture) publishBootstrap() {
	f.t.Helper()
	config := f.config()
	config.Status.DataSecretName = "edge-bootstrap"
	config.Status.Initialization = &configv1alpha1.VyOSConfigInitialization{DataSecretCreated: ptr.To(true)}
	if err := k8s.Status().Update(f.ctx, config); err != nil {
		f.t.Fatal(err)
	}
}

// machineReady plays the RouterMachine controller reporting a running server.
func (f *fixture) machineReady() {
	f.t.Helper()
	machine := f.machine()
	machine.Status.Addresses = []corev1alpha1.MachineAddress{{Type: corev1alpha1.AddressExternalIP, Address: "203.0.113.7"}}
	machine.Status.InfrastructureProvisioned = true
	conditions.True(&machine.Status.Conditions, machine.Generation, corev1alpha1.ReadyCondition, "ServerRunning", "")
	if err := k8s.Status().Update(f.ctx, machine); err != nil {
		f.t.Fatal(err)
	}
}

// configStatus plays the config provider reporting on the running router.
func (f *fixture) configStatus(applied, healthy bool) {
	f.t.Helper()
	config := f.config()
	config.Status.Version = "2026.10.07-0712-rolling"
	conditions.Set(&config.Status.Conditions, config.Generation, corev1alpha1.ConfigAppliedCondition, status(applied), "Reported", "")
	conditions.Set(&config.Status.Conditions, config.Generation, corev1alpha1.HealthyCondition, status(healthy), "Reported", "the API does not answer")
	if err := k8s.Status().Update(f.ctx, config); err != nil {
		f.t.Fatal(err)
	}
}

func status(ok bool) metav1.ConditionStatus {
	if ok {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

func (f *fixture) ready() {
	f.t.Helper()
	f.reconcile()
	f.publishBootstrap()
	f.machineReady()
	f.configStatus(true, true)
	f.reconcile()
}

func TestAdoptsMachineAndConfig(t *testing.T) {
	f := newFixture(t)

	f.reconcile()

	router := f.router()
	if !controllerutil.ContainsFinalizer(router, corev1alpha1.RouterFinalizer) {
		t.Error("no finalizer")
	}
	if router.Status.Phase != corev1alpha1.RouterPhasePending {
		t.Errorf("phase = %q", router.Status.Phase)
	}
	if !metav1.IsControlledBy(f.machine(), router) {
		t.Error("the Router does not own its RouterMachine")
	}
	if !metav1.IsControlledBy(f.config(), router) {
		t.Error("the Router does not own its config")
	}
	if conditions.IsTrue(router.Status.Conditions, corev1alpha1.BootstrapReadyCondition) {
		t.Error("BootstrapReady is True before the config provider published anything")
	}
}

func TestHandsBootstrapDataToTheMachine(t *testing.T) {
	f := newFixture(t)
	f.reconcile()
	if f.machine().Spec.Bootstrap.DataSecretName != nil {
		t.Fatal("the machine got bootstrap data that does not exist yet")
	}

	f.publishBootstrap()
	f.reconcile()

	if got := ptr.Deref(f.machine().Spec.Bootstrap.DataSecretName, ""); got != "edge-bootstrap" {
		t.Errorf("dataSecretName = %q", got)
	}
	router := f.router()
	if !conditions.IsTrue(router.Status.Conditions, corev1alpha1.BootstrapReadyCondition) {
		t.Error("BootstrapReady is not True")
	}
	if router.Status.Phase != corev1alpha1.RouterPhaseProvisioning {
		t.Errorf("phase = %q", router.Status.Phase)
	}
}

func TestBecomesReady(t *testing.T) {
	f := newFixture(t)
	f.reconcile()
	f.publishBootstrap()
	f.machineReady()
	f.reconcile()

	// The server is up, the configuration has not been applied yet.
	router := f.router()
	if router.Status.Phase != corev1alpha1.RouterPhaseConfiguring {
		t.Errorf("phase = %q, want Configuring", router.Status.Phase)
	}
	if conditions.IsTrue(router.Status.Conditions, corev1alpha1.ReadyCondition) {
		t.Error("Ready before the configuration was applied")
	}
	if len(router.Status.Addresses) != 1 {
		t.Errorf("addresses = %v: the config provider reads them from here", router.Status.Addresses)
	}

	f.configStatus(true, true)
	f.reconcile()

	router = f.router()
	if router.Status.Phase != corev1alpha1.RouterPhaseReady {
		t.Errorf("phase = %q, want Ready", router.Status.Phase)
	}
	for _, conditionType := range []string{
		corev1alpha1.ReadyCondition, corev1alpha1.MachineReadyCondition,
		corev1alpha1.ConfigAppliedCondition, corev1alpha1.HealthyCondition,
	} {
		if !conditions.IsTrue(router.Status.Conditions, conditionType) {
			t.Errorf("%s is not True", conditionType)
		}
	}
	if router.Status.Version != "2026.10.07-0712-rolling" {
		t.Errorf("version = %q", router.Status.Version)
	}
}

func TestDegrades(t *testing.T) {
	f := newFixture(t)
	f.ready()

	f.configStatus(true, false)
	f.reconcile()

	router := f.router()
	if router.Status.Phase != corev1alpha1.RouterPhaseDegraded {
		t.Errorf("phase = %q, want Degraded", router.Status.Phase)
	}
	ready := conditions.Get(router.Status.Conditions, corev1alpha1.ReadyCondition)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != ReasonUnhealthy {
		t.Errorf("Ready = %+v", ready)
	}
	if ready != nil && ready.Message == "" {
		t.Error("Ready carries no message: the reason the router is unhealthy is lost")
	}
}

func TestStaleConfigVerdictDoesNotCount(t *testing.T) {
	f := newFixture(t)
	f.ready()

	// The configuration changes. Until the provider has looked at the new
	// generation, "applied" refers to the old one.
	config := f.config()
	config.Spec.Commands = "set system host-name changed\n"
	if err := k8s.Update(f.ctx, config); err != nil {
		t.Fatal(err)
	}
	f.reconcile()

	router := f.router()
	if conditions.IsTrue(router.Status.Conditions, corev1alpha1.ConfigAppliedCondition) {
		t.Error("ConfigApplied is True for a configuration nobody applied yet")
	}
	// The router still runs the configuration it had and forwards traffic.
	// Calling it not ready would make every configuration change look like
	// an outage of one router.
	if !conditions.IsTrue(router.Status.Conditions, corev1alpha1.ReadyCondition) || router.Status.Phase != corev1alpha1.RouterPhaseReady {
		t.Errorf("Ready = %+v, phase = %s while a configuration change is outstanding",
			conditions.Get(router.Status.Conditions, corev1alpha1.ReadyCondition), router.Status.Phase)
	}
}

func TestARejectedConfigurationDoesNotTakeTheRouterOutOfService(t *testing.T) {
	f := newFixture(t)
	f.ready()

	// The config provider rolled a bad change back and says so.
	f.configStatus(false, true)
	f.reconcile()

	router := f.router()
	if conditions.IsTrue(router.Status.Conditions, corev1alpha1.ConfigAppliedCondition) {
		t.Error("ConfigApplied is True for a configuration the router refused")
	}
	// Otherwise the health check would reboot and then replace a router
	// that works, and the replacement would refuse the same configuration.
	if !conditions.IsTrue(router.Status.Conditions, corev1alpha1.ReadyCondition) {
		t.Errorf("Ready = %+v", conditions.Get(router.Status.Conditions, corev1alpha1.ReadyCondition))
	}
}

func TestDrain(t *testing.T) {
	f := newFixture(t)
	f.ready()

	router := f.router()
	router.Annotations = map[string]string{corev1alpha1.DrainAnnotation: "rollout"}
	if err := k8s.Update(f.ctx, router); err != nil {
		t.Fatal(err)
	}
	f.reconcile()

	if got := f.config().Annotations[corev1alpha1.DrainAnnotation]; got != "rollout" {
		t.Fatalf("drain annotation on the config = %q", got)
	}
	router = f.router()
	if router.Status.Phase != corev1alpha1.RouterPhaseDraining {
		t.Errorf("phase = %q, want Draining", router.Status.Phase)
	}
	if conditions.IsTrue(router.Status.Conditions, corev1alpha1.DrainedCondition) {
		t.Error("Drained before the provider said so")
	}

	config := f.config()
	conditions.True(&config.Status.Conditions, config.Generation, corev1alpha1.DrainedCondition, "VRRPBackup", "")
	if err := k8s.Status().Update(f.ctx, config); err != nil {
		t.Fatal(err)
	}
	f.reconcile()
	if !conditions.IsTrue(f.router().Status.Conditions, corev1alpha1.DrainedCondition) {
		t.Error("Drained is not True")
	}

	// Taking the annotation off again takes it off the config.
	router = f.router()
	router.Annotations = nil
	if err := k8s.Update(f.ctx, router); err != nil {
		t.Fatal(err)
	}
	f.reconcile()
	if _, ok := f.config().Annotations[corev1alpha1.DrainAnnotation]; ok {
		t.Error("the config is still asked to drain")
	}
}

func TestMissingConfig(t *testing.T) {
	f := newFixture(t)
	if err := k8s.Delete(f.ctx, f.config()); err != nil {
		t.Fatal(err)
	}

	result := f.reconcile()

	if result.RequeueAfter == 0 {
		t.Error("no requeue")
	}
	ready := conditions.Get(f.router().Status.Conditions, corev1alpha1.ReadyCondition)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != ReasonConfigNotFound {
		t.Errorf("Ready = %+v", ready)
	}
}

func TestDeletionRemovesTheServerBeforeTheConfig(t *testing.T) {
	f := newFixture(t)
	f.ready()

	// Stand in for the machine controller's finalizer.
	machine := f.machine()
	controllerutil.AddFinalizer(machine, corev1alpha1.RouterMachineFinalizer)
	if err := k8s.Update(f.ctx, machine); err != nil {
		t.Fatal(err)
	}

	if err := k8s.Delete(f.ctx, f.router()); err != nil {
		t.Fatal(err)
	}
	f.reconcile()

	if f.machine().DeletionTimestamp.IsZero() {
		t.Fatal("the machine was not deleted")
	}
	// The config holds the router's keys and its certificate. While the
	// server exists it has to stay, or a still running router would be left
	// with credentials nobody can revoke or use.
	if !f.config().DeletionTimestamp.IsZero() {
		t.Fatal("the config was deleted while the server still exists")
	}
	if f.router().Status.Phase != corev1alpha1.RouterPhaseDeleting {
		t.Errorf("phase = %q", f.router().Status.Phase)
	}

	machine = f.machine()
	controllerutil.RemoveFinalizer(machine, corev1alpha1.RouterMachineFinalizer)
	if err := k8s.Update(f.ctx, machine); err != nil {
		t.Fatal(err)
	}
	f.reconcile()
	f.reconcile()

	for _, object := range []client.Object{&corev1alpha1.Router{}, &configv1alpha1.VyOSConfig{}} {
		if err := k8s.Get(f.ctx, f.key(), object); !apierrors.IsNotFound(err) {
			t.Errorf("%T still exists: %v", object, err)
		}
	}
}
