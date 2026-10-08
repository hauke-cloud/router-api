package routerdeployment

import (
	"context"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

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
	r         *Reconciler
}

// newFixture creates the two templates and a RouterDeployment named edge.
// replacementHash is what the config provider has published on its template;
// empty means it has not looked at it yet.
func newFixture(t *testing.T, replicas int32, replacementHash string) *fixture {
	t.Helper()
	f := &fixture{t: t, ctx: context.Background(), namespace: testenv.Namespace(t, k8s), r: &Reconciler{Client: k8s}}

	labels := map[string]string{"app": "edge"}
	configTemplate := &configv1alpha1.VyOSConfigTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: f.namespace},
		Spec: configv1alpha1.VyOSConfigTemplateSpec{Template: configv1alpha1.VyOSConfigTemplateResource{
			Spec: configv1alpha1.VyOSConfigSpec{Image: "ghcr.io/hauke-cloud/vyos:1", Commands: "set system time-zone UTC\n"},
		}},
	}
	objects := []client.Object{
		&infrav1alpha1.HetznerMachineTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: f.namespace},
			Spec: infrav1alpha1.HetznerMachineTemplateSpec{Template: infrav1alpha1.HetznerMachineTemplateResource{
				Spec: infrav1alpha1.HetznerMachineSpec{
					NetworkRef: corev1alpha1.LocalObjectReference{Name: "net"}, ServerType: "cx23", Location: "fsn1",
				},
			}},
		},
		configTemplate,
		&corev1alpha1.RouterDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: f.namespace},
			Spec: corev1alpha1.RouterDeploymentSpec{
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
	if replacementHash != "" {
		f.publishReplacementHash(replacementHash)
	}
	return f
}

func (f *fixture) key() types.NamespacedName {
	return types.NamespacedName{Namespace: f.namespace, Name: "edge"}
}

func (f *fixture) publishReplacementHash(hash string) {
	f.t.Helper()
	template := &configv1alpha1.VyOSConfigTemplate{}
	if err := k8s.Get(f.ctx, f.key(), template); err != nil {
		f.t.Fatal(err)
	}
	template.Status.ReplacementHash = hash
	template.Status.ObservedGeneration = template.Generation
	if err := k8s.Status().Update(f.ctx, template); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) reconcile() reconcile.Result {
	f.t.Helper()
	result, err := f.r.Reconcile(f.ctx, reconcile.Request{NamespacedName: f.key()})
	if err != nil {
		f.t.Fatalf("Reconcile: %v", err)
	}
	return result
}

func (f *fixture) deployment() *corev1alpha1.RouterDeployment {
	f.t.Helper()
	deployment := &corev1alpha1.RouterDeployment{}
	if err := k8s.Get(f.ctx, f.key(), deployment); err != nil {
		f.t.Fatal(err)
	}
	return deployment
}

// sets returns the RouterSets ordered by revision, oldest first.
func (f *fixture) sets() []corev1alpha1.RouterSet {
	f.t.Helper()
	list := &corev1alpha1.RouterSetList{}
	if err := k8s.List(f.ctx, list, client.InNamespace(f.namespace)); err != nil {
		f.t.Fatal(err)
	}
	slices.SortFunc(list.Items, func(a, b corev1alpha1.RouterSet) int { return revision(&a) - revision(&b) })
	return list.Items
}

// replicas returns spec.replicas of every set, oldest first, as "2,1".
func (f *fixture) replicas() string {
	f.t.Helper()
	sets := f.sets()
	parts := make([]string, len(sets))
	for i := range sets {
		parts[i] = strconv.Itoa(int(ptr.Deref(sets[i].Spec.Replicas, -1)))
	}
	return strings.Join(parts, ",")
}

// settle plays the RouterSet controller: every set has as many routers as its
// spec asks for, and available of them are available (-1 for all).
func (f *fixture) settle(revisionNumber int, available int32) {
	f.t.Helper()
	sets := f.sets()
	for i := range sets {
		set := &sets[i]
		want := ptr.Deref(set.Spec.Replicas, 0)
		status := corev1alpha1.RouterSetStatus{
			Replicas: want, ReadyReplicas: want, AvailableReplicas: want, UpToDateReplicas: want,
			ObservedGeneration: set.Generation,
		}
		if revision(set) == revisionNumber && available >= 0 {
			status.ReadyReplicas, status.AvailableReplicas = available, available
		}
		set.Status = status
		if err := k8s.Status().Update(f.ctx, set); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *fixture) changeServerType(serverType string) {
	f.t.Helper()
	template := &infrav1alpha1.HetznerMachineTemplate{}
	if err := k8s.Get(f.ctx, f.key(), template); err != nil {
		f.t.Fatal(err)
	}
	template.Spec.Template.Spec.ServerType = serverType
	if err := k8s.Update(f.ctx, template); err != nil {
		f.t.Fatal(err)
	}
}

func TestCreatesARouterSet(t *testing.T) {
	f := newFixture(t, 2, "h1")

	f.reconcile()

	sets := f.sets()
	if len(sets) != 1 {
		t.Fatalf("%d RouterSets, want 1", len(sets))
	}
	set := &sets[0]
	deployment := f.deployment()
	if !metav1.IsControlledBy(set, deployment) {
		t.Error("the set is not owned by the deployment")
	}
	hash := set.Labels[corev1alpha1.TemplateHashLabel]
	if hash == "" || set.Name != "edge-"+hash {
		t.Errorf("name = %q, hash label = %q", set.Name, hash)
	}
	if set.Labels[corev1alpha1.DeploymentNameLabel] != "edge" {
		t.Errorf("labels = %v", set.Labels)
	}
	// Without the hash in the selector the sets of a rollout would each
	// count the other's routers as their own.
	if set.Spec.Selector.MatchLabels[corev1alpha1.TemplateHashLabel] != hash || set.Spec.Selector.MatchLabels["app"] != "edge" {
		t.Errorf("selector = %v", set.Spec.Selector.MatchLabels)
	}
	if set.Spec.Template.Labels[corev1alpha1.TemplateHashLabel] != hash {
		t.Errorf("template labels = %v", set.Spec.Template.Labels)
	}
	if got := ptr.Deref(set.Spec.Replicas, 0); got != 2 {
		t.Errorf("replicas = %d", got)
	}
	if set.Spec.MinReadySeconds != 30 {
		t.Errorf("minReadySeconds = %d, want the deployment's default of 30", set.Spec.MinReadySeconds)
	}
	if revision(set) != 1 {
		t.Errorf("revision = %d", revision(set))
	}

	f.reconcile()
	if got := len(f.sets()); got != 1 {
		t.Errorf("%d sets after a second reconcile", got)
	}
}

func TestWaitsForTheConfigProvider(t *testing.T) {
	f := newFixture(t, 2, "")

	result := f.reconcile()

	// The replacement hash is part of what identifies a revision. Starting
	// without it would mean replacing every router the moment it appears.
	if got := len(f.sets()); got != 0 {
		t.Fatalf("%d sets created before the config provider published its hash", got)
	}
	if result.RequeueAfter == 0 {
		t.Error("no requeue")
	}
	available := conditions.Get(f.deployment().Status.Conditions, corev1alpha1.AvailableCondition)
	if available == nil || available.Reason != ReasonWaitingForProvider {
		t.Errorf("Available = %+v", available)
	}
}

func TestRollingReplacement(t *testing.T) {
	f := newFixture(t, 2, "h1")
	f.reconcile()
	f.settle(0, -1)
	f.reconcile()
	if conditions.IsTrue(f.deployment().Status.Conditions, corev1alpha1.RollingOutCondition) {
		t.Fatal("RollingOut before anything changed")
	}

	f.changeServerType("cx33")

	// Each step: what the deployment asks for, then the sets catching up.
	// With maxSurge 1 and maxUnavailable 0 there are never fewer than two
	// available routers and never more than three in total.
	steps := []struct {
		newAvailable int32
		want         string
		why          string
	}{
		{0, "2,1", "one new router is added; nothing old is touched before it works"},
		{0, "2,1", "the new router is not available yet"},
		{1, "1,1", "the new router works, one old one can go"},
		{1, "1,2", "room for the second new router"},
		{1, "1,2", "the second new router is not available yet"},
		{2, "0,2", "both new routers work, the last old one can go"},
	}
	for i, step := range steps {
		f.reconcile()
		if i == 0 {
			if !conditions.IsTrue(f.deployment().Status.Conditions, corev1alpha1.RollingOutCondition) {
				t.Error("RollingOut is not True during the rollout")
			}
		}
		f.settle(2, step.newAvailable)
		f.reconcile()
		if got := f.replicas(); got != step.want {
			t.Fatalf("step %d: replicas = %s, want %s (%s)", i, got, step.want, step.why)
		}
		f.settle(2, step.newAvailable)
	}

	f.settle(0, -1)
	f.reconcile()
	deployment := f.deployment()
	if conditions.IsTrue(deployment.Status.Conditions, corev1alpha1.RollingOutCondition) {
		t.Error("RollingOut is still True")
	}
	status := deployment.Status
	if status.Replicas != 2 || status.UpdatedReplicas != 2 || status.AvailableReplicas != 2 {
		t.Errorf("status = %+v", status)
	}
	if revision(&f.sets()[1]) != 2 {
		t.Errorf("revision of the new set = %d", revision(&f.sets()[1]))
	}
}

func TestReplacementHashChangeReplaces(t *testing.T) {
	f := newFixture(t, 2, "h1")
	f.reconcile()
	f.settle(0, -1)

	f.publishReplacementHash("h2")
	f.reconcile()

	if got := len(f.sets()); got != 2 {
		t.Errorf("%d sets, want a new one for the new image", got)
	}
}

func TestConfigOnlyChangeDoesNotReplace(t *testing.T) {
	f := newFixture(t, 2, "h1")
	f.reconcile()
	f.settle(0, -1)

	template := &configv1alpha1.VyOSConfigTemplate{}
	if err := k8s.Get(f.ctx, f.key(), template); err != nil {
		t.Fatal(err)
	}
	template.Spec.Template.Spec.Commands = "set system time-zone Europe/Berlin\n"
	if err := k8s.Update(f.ctx, template); err != nil {
		t.Fatal(err)
	}
	f.reconcile()

	// The hash the provider published is unchanged: this is for the
	// RouterSet to apply to the running routers.
	if got := len(f.sets()); got != 1 {
		t.Errorf("%d sets: a firewall rule must not cost a server", got)
	}
}

func TestScaling(t *testing.T) {
	f := newFixture(t, 2, "h1")
	f.reconcile()
	f.settle(0, -1)

	for _, replicas := range []int32{3, 1} {
		deployment := f.deployment()
		deployment.Spec.Replicas = ptr.To(replicas)
		if err := k8s.Update(f.ctx, deployment); err != nil {
			t.Fatal(err)
		}
		f.reconcile()
		if got := f.replicas(); got != strconv.Itoa(int(replicas)) {
			t.Errorf("replicas = %s, want %d", got, replicas)
		}
		f.settle(0, -1)
	}
}

func TestPausedDoesNotRollOut(t *testing.T) {
	f := newFixture(t, 2, "h1")
	f.reconcile()
	f.settle(0, -1)

	deployment := f.deployment()
	deployment.Spec.Paused = true
	if err := k8s.Update(f.ctx, deployment); err != nil {
		t.Fatal(err)
	}
	f.changeServerType("cx33")
	f.reconcile()

	if got := len(f.sets()); got != 1 {
		t.Errorf("%d sets: a paused deployment started a rollout", got)
	}
	if !conditions.IsTrue(f.deployment().Status.Conditions, corev1alpha1.PausedCondition) {
		t.Error("Paused is not True")
	}
}

func TestHistoryIsTrimmed(t *testing.T) {
	f := newFixture(t, 1, "h1")
	deployment := f.deployment()
	deployment.Spec.RevisionHistoryLimit = ptr.To(int32(1))
	if err := k8s.Update(f.ctx, deployment); err != nil {
		t.Fatal(err)
	}
	f.reconcile()
	f.settle(0, -1)

	// Three rollouts, each run to completion.
	for _, serverType := range []string{"cx33", "cx43", "cx53"} {
		f.changeServerType(serverType)
		for range 6 {
			f.reconcile()
			f.settle(0, -1)
		}
	}

	sets := f.sets()
	if len(sets) != 2 {
		t.Fatalf("%d sets, want the current one and 1 of history", len(sets))
	}
	if got := f.replicas(); got != "0,1" {
		t.Errorf("replicas = %s", got)
	}
	if revision(&sets[1]) != 4 {
		t.Errorf("newest revision = %d, want 4", revision(&sets[1]))
	}
}
