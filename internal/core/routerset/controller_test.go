package routerset

import (
	"context"
	"os"
	"slices"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
	now       time.Time
	r         *Reconciler
}

// newFixture creates the two templates and a RouterSet named edge.
func newFixture(t *testing.T, replicas int32) *fixture {
	t.Helper()
	f := &fixture{t: t, ctx: context.Background(), namespace: testenv.Namespace(t, k8s), now: time.Now()}
	f.r = &Reconciler{Client: k8s, Now: func() time.Time { return f.now }}

	labels := map[string]string{"app": "edge"}
	objects := []client.Object{
		&infrav1alpha1.HetznerMachineTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: f.namespace},
			Spec: infrav1alpha1.HetznerMachineTemplateSpec{Template: infrav1alpha1.HetznerMachineTemplateResource{
				Spec: infrav1alpha1.HetznerMachineSpec{
					NetworkRef: corev1alpha1.LocalObjectReference{Name: "net"}, ServerType: "cx23", Location: "fsn1",
				},
			}},
		},
		&configv1alpha1.VyOSConfigTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: f.namespace},
			Spec: configv1alpha1.VyOSConfigTemplateSpec{Template: configv1alpha1.VyOSConfigTemplateResource{
				Spec: configv1alpha1.VyOSConfigSpec{Image: "ghcr.io/hauke-cloud/vyos:test", Commands: "set system time-zone UTC\n"},
			}},
		},
		&corev1alpha1.RouterSet{
			ObjectMeta: metav1.ObjectMeta{
				Name: "edge", Namespace: f.namespace,
				Labels: map[string]string{corev1alpha1.DeploymentNameLabel: "edge", corev1alpha1.TemplateHashLabel: "abc"},
			},
			Spec: corev1alpha1.RouterSetSpec{
				Replicas: ptr.To(replicas),
				Selector: metav1.LabelSelector{MatchLabels: labels},
				Template: corev1alpha1.RouterTemplateSpec{
					ObjectMeta: corev1alpha1.ObjectMeta{Labels: labels},
					Spec: corev1alpha1.RouterTemplate{
						InfrastructureTemplateRef: corev1alpha1.ContractReference{
							APIGroup: infrav1alpha1.GroupVersion.Group, Kind: "HetznerMachineTemplate", Name: "edge",
						},
						ConfigTemplateRef: corev1alpha1.ContractReference{
							APIGroup: configv1alpha1.GroupVersion.Group, Kind: "VyOSConfigTemplate", Name: "edge",
						},
					},
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

func (f *fixture) reconcile() reconcile.Result {
	f.t.Helper()
	result, err := f.r.Reconcile(f.ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: f.namespace, Name: "edge"}})
	if err != nil {
		f.t.Fatalf("Reconcile: %v", err)
	}
	return result
}

func (f *fixture) set() *corev1alpha1.RouterSet {
	f.t.Helper()
	set := &corev1alpha1.RouterSet{}
	if err := k8s.Get(f.ctx, types.NamespacedName{Namespace: f.namespace, Name: "edge"}, set); err != nil {
		f.t.Fatal(err)
	}
	return set
}

func (f *fixture) scale(replicas int32) {
	f.t.Helper()
	set := f.set()
	set.Spec.Replicas = ptr.To(replicas)
	if err := k8s.Update(f.ctx, set); err != nil {
		f.t.Fatal(err)
	}
}

// routers returns the routers that are not being deleted, sorted by name.
func (f *fixture) routers() []corev1alpha1.Router {
	f.t.Helper()
	list := &corev1alpha1.RouterList{}
	if err := k8s.List(f.ctx, list, client.InNamespace(f.namespace)); err != nil {
		f.t.Fatal(err)
	}
	routers := slices.DeleteFunc(list.Items, func(r corev1alpha1.Router) bool { return !r.DeletionTimestamp.IsZero() })
	slices.SortFunc(routers, func(a, b corev1alpha1.Router) int { return cmpString(a.Name, b.Name) })
	return routers
}

func cmpString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func (f *fixture) config(name string) *configv1alpha1.VyOSConfig {
	f.t.Helper()
	config := &configv1alpha1.VyOSConfig{}
	if err := k8s.Get(f.ctx, types.NamespacedName{Namespace: f.namespace, Name: name}, config); err != nil {
		f.t.Fatal(err)
	}
	return config
}

// setReady plays the Router controller. since is when the router became (or
// stopped being) ready.
func (f *fixture) setReady(name string, ready bool, since time.Time) {
	f.t.Helper()
	router := &corev1alpha1.Router{}
	if err := k8s.Get(f.ctx, types.NamespacedName{Namespace: f.namespace, Name: name}, router); err != nil {
		f.t.Fatal(err)
	}
	status := metav1.ConditionFalse
	if ready {
		status = metav1.ConditionTrue
	}
	router.Status.Conditions = slices.DeleteFunc(router.Status.Conditions, func(c metav1.Condition) bool { return c.Type == corev1alpha1.ReadyCondition })
	router.Status.Conditions = append(router.Status.Conditions, metav1.Condition{
		Type: corev1alpha1.ReadyCondition, Status: status, Reason: "Test",
		ObservedGeneration: router.Generation, LastTransitionTime: metav1.NewTime(since),
	})
	// The Router's own copy of ConfigApplied stays True whatever happens to
	// the config object: it is only brought up to date when the Router is
	// reconciled, and the set must not go by it.
	if ready {
		conditions.True(&router.Status.Conditions, router.Generation, corev1alpha1.ConfigAppliedCondition, "Test", "")
	}
	if err := k8s.Status().Update(f.ctx, router); err != nil {
		f.t.Fatal(err)
	}
	if ready {
		// A router does not become ready without having been configured.
		if config := f.config(name); conditions.Get(config.Status.Conditions, corev1alpha1.ConfigAppliedCondition) == nil {
			f.setApplied(name, true)
		}
	}
}

// setApplied plays the config provider reporting, for the config object as
// it is now, whether the router runs it.
func (f *fixture) setApplied(name string, applied bool) {
	f.t.Helper()
	config := f.config(name)
	status := metav1.ConditionFalse
	if applied {
		status = metav1.ConditionTrue
	}
	conditions.Set(&config.Status.Conditions, config.Generation, corev1alpha1.ConfigAppliedCondition, status, "Test", "")
	if err := k8s.Status().Update(f.ctx, config); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) setDrained(name string) {
	f.t.Helper()
	router := &corev1alpha1.Router{}
	if err := k8s.Get(f.ctx, types.NamespacedName{Namespace: f.namespace, Name: name}, router); err != nil {
		f.t.Fatal(err)
	}
	conditions.True(&router.Status.Conditions, router.Generation, corev1alpha1.DrainedCondition, "Test", "")
	if err := k8s.Status().Update(f.ctx, router); err != nil {
		f.t.Fatal(err)
	}
}

// allReady creates the routers and marks every one of them ready for an hour.
func (f *fixture) allReady() []corev1alpha1.Router {
	f.t.Helper()
	f.reconcile()
	for _, name := range names(f.routers()) {
		f.setReady(name, true, f.now.Add(-time.Hour))
	}
	f.reconcile()
	return f.routers()
}

func (f *fixture) changeCommands(commands string) {
	f.t.Helper()
	template := &configv1alpha1.VyOSConfigTemplate{}
	if err := k8s.Get(f.ctx, types.NamespacedName{Namespace: f.namespace, Name: "edge"}, template); err != nil {
		f.t.Fatal(err)
	}
	template.Spec.Template.Spec.Commands = commands
	if err := k8s.Update(f.ctx, template); err != nil {
		f.t.Fatal(err)
	}
}

// updated returns the routers whose config object carries the commands.
func (f *fixture) updated(commands string) []string {
	f.t.Helper()
	var updated []string
	for _, name := range names(f.routers()) {
		if f.config(name).Spec.Commands == commands {
			updated = append(updated, name)
		}
	}
	return updated
}

func TestCreatesRouters(t *testing.T) {
	f := newFixture(t, 2)

	f.reconcile()

	routers := f.routers()
	if len(routers) != 2 {
		t.Fatalf("%d routers, want 2", len(routers))
	}
	set := f.set()
	for _, router := range routers {
		key := types.NamespacedName{Namespace: f.namespace, Name: router.Name}
		if !metav1.IsControlledBy(&router, set) {
			t.Errorf("%s is not owned by the set", router.Name)
		}
		wantLabels := map[string]string{
			"app":                            "edge",
			corev1alpha1.SetNameLabel:        "edge",
			corev1alpha1.DeploymentNameLabel: "edge",
			corev1alpha1.TemplateHashLabel:   "abc",
			corev1alpha1.RouterNameLabel:     router.Name,
		}
		for k, v := range wantLabels {
			if router.Labels[k] != v {
				t.Errorf("%s: label %s = %q, want %q", router.Name, k, router.Labels[k], v)
			}
		}

		machine := &corev1alpha1.RouterMachine{}
		infra := &infrav1alpha1.HetznerMachine{}
		config := &configv1alpha1.VyOSConfig{}
		for _, object := range []client.Object{machine, infra, config} {
			if err := k8s.Get(f.ctx, key, object); err != nil {
				t.Fatalf("%s: get %T: %v", router.Name, object, err)
			}
			// Peers find each other through this label.
			if object.GetLabels()[corev1alpha1.DeploymentNameLabel] != "edge" {
				t.Errorf("%s: %T has no deployment label", router.Name, object)
			}
		}
		if router.Spec.MachineRef.Name != router.Name || router.Spec.ConfigRef.Kind != "VyOSConfig" || router.Spec.ConfigRef.Name != router.Name {
			t.Errorf("%s: spec = %+v", router.Name, router.Spec)
		}
		if machine.Spec.InfrastructureRef.Kind != "HetznerMachine" || machine.Spec.InfrastructureRef.Name != router.Name {
			t.Errorf("%s: infrastructureRef = %+v", router.Name, machine.Spec.InfrastructureRef)
		}
		if infra.Spec.ServerType != "cx23" || config.Spec.Commands != "set system time-zone UTC\n" {
			t.Errorf("%s: the provider objects do not carry the template's spec", router.Name)
		}
		if !metav1.IsControlledBy(machine, &router) || !metav1.IsControlledBy(config, &router) {
			t.Errorf("%s: machine and config are not owned by the router", router.Name)
		}
	}

	if got := set.Status.Replicas; got != 2 {
		t.Errorf("status.replicas = %d", got)
	}
	if set.Status.ReadyReplicas != 0 || set.Status.AvailableReplicas != 0 {
		t.Errorf("status = %+v: nothing is ready yet", set.Status)
	}

	// A second pass must not create more.
	f.reconcile()
	if got := len(f.routers()); got != 2 {
		t.Errorf("%d routers after a second reconcile", got)
	}
}

func TestRepairsAHalfCreatedRouter(t *testing.T) {
	f := newFixture(t, 1)
	f.reconcile()
	name := f.routers()[0].Name
	key := types.NamespacedName{Namespace: f.namespace, Name: name}

	// As if the controller had died between creating the Router and its
	// parts.
	for _, object := range []client.Object{&corev1alpha1.RouterMachine{}, &infrav1alpha1.HetznerMachine{}} {
		if err := k8s.Get(f.ctx, key, object); err != nil {
			t.Fatal(err)
		}
		if err := k8s.Delete(f.ctx, object); err != nil {
			t.Fatal(err)
		}
	}

	f.reconcile()

	for _, object := range []client.Object{&corev1alpha1.RouterMachine{}, &infrav1alpha1.HetznerMachine{}} {
		if err := k8s.Get(f.ctx, key, object); err != nil {
			t.Errorf("%T was not recreated: %v", object, err)
		}
	}
	if got := len(f.routers()); got != 1 {
		t.Errorf("%d routers, want 1", got)
	}
}

func TestStatusCountsAvailability(t *testing.T) {
	f := newFixture(t, 2)
	set := f.set()
	set.Spec.MinReadySeconds = 60
	if err := k8s.Update(f.ctx, set); err != nil {
		t.Fatal(err)
	}
	f.reconcile()
	routers := f.routers()
	f.setReady(routers[0].Name, true, f.now.Add(-2*time.Minute))
	f.setReady(routers[1].Name, true, f.now.Add(-10*time.Second))

	result := f.reconcile()

	status := f.set().Status
	if status.ReadyReplicas != 2 || status.AvailableReplicas != 1 || status.UpToDateReplicas != 2 {
		t.Errorf("status = %+v, want 2 ready, 1 available, 2 up to date", status)
	}
	if conditions.IsTrue(status.Conditions, corev1alpha1.AvailableCondition) {
		t.Error("Available with one of two routers still settling")
	}
	if result.RequeueAfter == 0 || result.RequeueAfter > time.Minute {
		t.Errorf("RequeueAfter = %v, want a wake-up for when the second router has settled", result.RequeueAfter)
	}

	f.now = f.now.Add(time.Minute)
	f.reconcile()
	status = f.set().Status
	if status.AvailableReplicas != 2 || !conditions.IsTrue(status.Conditions, corev1alpha1.AvailableCondition) {
		t.Errorf("status = %+v, want everything available", status)
	}
}

func TestScaleDownTakesTheBrokenRouterFirst(t *testing.T) {
	f := newFixture(t, 2)
	routers := f.allReady()
	f.setReady(routers[1].Name, false, f.now)

	f.scale(1)
	f.reconcile()

	remaining := f.routers()
	if len(remaining) != 1 || remaining[0].Name != routers[0].Name {
		t.Fatalf("remaining = %v, want only the healthy %s", names(remaining), routers[0].Name)
	}
}

func names(routers []corev1alpha1.Router) []string {
	out := make([]string, len(routers))
	for i := range routers {
		out[i] = routers[i].Name
	}
	return out
}

func TestScaleDownDrainsAWorkingRouterFirst(t *testing.T) {
	f := newFixture(t, 2)
	f.allReady()

	f.scale(1)
	f.reconcile()

	// Nothing is deleted yet: one router has been asked to hand over.
	routers := f.routers()
	if len(routers) != 2 {
		t.Fatalf("%d routers, want both still there while one drains", len(routers))
	}
	var draining string
	for _, router := range routers {
		if _, ok := router.Annotations[corev1alpha1.DrainAnnotation]; ok {
			if draining != "" {
				t.Fatal("both routers were asked to drain")
			}
			draining = router.Name
		}
	}
	if draining == "" {
		t.Fatal("no router was asked to drain")
	}

	f.reconcile()
	if len(f.routers()) != 2 {
		t.Fatal("the router was deleted before it reported being drained")
	}

	f.setDrained(draining)
	f.reconcile()

	remaining := f.routers()
	if len(remaining) != 1 || remaining[0].Name == draining {
		t.Errorf("remaining = %v, want the one that did not drain", names(remaining))
	}
}

func TestScaleDownGivesUpWaitingForADrain(t *testing.T) {
	f := newFixture(t, 2)
	f.allReady()
	f.scale(1)
	f.reconcile()

	f.now = f.now.Add(DrainTimeout + time.Second)
	f.reconcile()

	if got := len(f.routers()); got != 1 {
		t.Errorf("%d routers: a router that never reports being drained blocks the scale-down forever", got)
	}
}

func TestConfigChangeGoesToOneRouterAtATime(t *testing.T) {
	const changed = "set system time-zone Europe/Berlin\n"
	f := newFixture(t, 2)
	f.allReady()

	f.changeCommands(changed)
	f.reconcile()

	first := f.updated(changed)
	if len(first) != 1 {
		t.Fatalf("%d configs updated, want exactly 1", len(first))
	}

	// The router has the new configuration and has not applied it: the
	// config provider's verdict is still the one on the configuration from
	// before, and the Router still mirrors that as True. This is exactly
	// what the set sees for the first seconds after handing out a change
	// (found on Hetzner, where both routers got a change within seconds of
	// each other). The other router must not be touched.
	f.reconcile()
	f.reconcile()
	if got := f.updated(changed); len(got) != 1 {
		t.Fatalf("updated %v right after handing the change to %s, before anything says it was applied", got, first[0])
	}

	// The provider is at it. The router still forwards traffic on the old
	// configuration, so it stays ready. But until it runs the new one the
	// other router stays as it is: if the change is bad, one router has to
	// be left that has not seen it.
	f.setApplied(first[0], false)
	f.reconcile()
	f.reconcile()
	if got := f.updated(changed); len(got) != 1 {
		t.Fatalf("updated %v while %s has not applied the change", got, first[0])
	}
	if got := f.set().Status.UpToDateReplicas; got != 0 {
		t.Errorf("upToDateReplicas = %d: having a configuration is not running it", got)
	}
	if got := f.set().Status.ReadyReplicas; got != 2 {
		t.Errorf("readyReplicas = %d: a router that is applying a change is still in service", got)
	}

	f.setApplied(first[0], true)
	f.reconcile()
	if got := f.updated(changed); len(got) != 2 {
		t.Errorf("updated = %v, want both", got)
	}
	if got := f.set().Status.UpToDateReplicas; got != 1 {
		t.Errorf("upToDateReplicas = %d, want 1: the second router has not applied it yet", got)
	}
}

func TestARefusedConfigChangeStopsAtTheFirstRouter(t *testing.T) {
	const changed = "set firewall bogus\n"
	f := newFixture(t, 2)
	f.allReady()
	f.changeCommands(changed)
	f.reconcile()
	first := f.updated(changed)

	// The router refuses the change and keeps running what it had.
	f.setApplied(first[0], false)
	for range 3 {
		f.reconcile()
	}

	if got := f.updated(changed); len(got) != 1 {
		t.Errorf("updated = %v: a configuration one router refused was handed to the next", got)
	}
}

func TestConfigChangeReachesABrokenRouterFirst(t *testing.T) {
	const changed = "set system time-zone Europe/Berlin\n"
	f := newFixture(t, 2)
	routers := f.allReady()
	f.setReady(routers[1].Name, false, f.now)

	f.changeCommands(changed)
	f.reconcile()

	// A router that is already down loses nothing by being changed, and the
	// change may be what fixes it.
	if got := f.updated(changed); len(got) != 1 || got[0] != routers[1].Name {
		t.Errorf("updated = %v, want the broken %s", got, routers[1].Name)
	}
}

func TestPaused(t *testing.T) {
	f := newFixture(t, 2)
	set := f.set()
	set.Annotations = map[string]string{corev1alpha1.PausedAnnotation: "true"}
	if err := k8s.Update(f.ctx, set); err != nil {
		t.Fatal(err)
	}

	f.reconcile()

	if got := len(f.routers()); got != 0 {
		t.Errorf("a paused set created %d routers", got)
	}
	if !conditions.IsTrue(f.set().Status.Conditions, corev1alpha1.PausedCondition) {
		t.Error("Paused is not True")
	}
}

func TestGoneSet(t *testing.T) {
	f := newFixture(t, 1)
	if err := k8s.Delete(f.ctx, f.set()); err != nil {
		t.Fatal(err)
	}
	result, err := f.r.Reconcile(f.ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: f.namespace, Name: "edge"}})
	if err != nil || result.RequeueAfter != 0 {
		t.Errorf("result = %+v, err = %v", result, err)
	}
	if err := k8s.Get(f.ctx, types.NamespacedName{Namespace: f.namespace, Name: "edge"}, &corev1alpha1.RouterSet{}); !apierrors.IsNotFound(err) {
		t.Errorf("err = %v", err)
	}
}
