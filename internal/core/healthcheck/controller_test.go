package healthcheck

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
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

// newFixture creates a RouterHealthCheck named edge and the given routers,
// each with its RouterMachine and owned by a RouterSet, all ready for an hour.
func newFixture(t *testing.T, routers ...string) *fixture {
	t.Helper()
	f := &fixture{t: t, ctx: context.Background(), namespace: testenv.Namespace(t, k8s), now: time.Now()}
	f.r = &Reconciler{Client: k8s, Now: func() time.Time { return f.now }}

	check := &corev1alpha1.RouterHealthCheck{
		ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: f.namespace},
		Spec:       corev1alpha1.RouterHealthCheckSpec{Selector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "edge"}}},
	}
	if err := k8s.Create(f.ctx, check); err != nil {
		t.Fatal(err)
	}
	set := &corev1alpha1.RouterSet{
		ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: f.namespace},
		Spec: corev1alpha1.RouterSetSpec{
			Selector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "edge"}},
			Template: corev1alpha1.RouterTemplateSpec{
				ObjectMeta: corev1alpha1.ObjectMeta{Labels: map[string]string{"app": "edge"}},
				Spec: corev1alpha1.RouterTemplate{
					InfrastructureTemplateRef: corev1alpha1.ContractReference{APIGroup: infrav1alpha1.GroupVersion.Group, Kind: "HetznerMachineTemplate", Name: "edge"},
					ConfigTemplateRef:         corev1alpha1.ContractReference{APIGroup: configv1alpha1.GroupVersion.Group, Kind: "VyOSConfigTemplate", Name: "edge"},
				},
			},
		},
	}
	if err := k8s.Create(f.ctx, set); err != nil {
		t.Fatal(err)
	}
	for _, name := range routers {
		f.createRouter(name, set)
		f.setReady(name, metav1.ConditionTrue, corev1alpha1.RouterPhaseReady, time.Hour)
	}
	return f
}

func (f *fixture) createRouter(name string, owner *corev1alpha1.RouterSet) {
	f.t.Helper()
	router := &corev1alpha1.Router{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.namespace, Labels: map[string]string{"app": "edge"}},
		Spec: corev1alpha1.RouterSpec{
			MachineRef: corev1alpha1.LocalObjectReference{Name: name},
			ConfigRef:  corev1alpha1.ContractReference{APIGroup: configv1alpha1.GroupVersion.Group, Kind: "VyOSConfig", Name: name},
		},
	}
	if owner != nil {
		router.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(owner, corev1alpha1.GroupVersion.WithKind("RouterSet"))}
	}
	machine := &corev1alpha1.RouterMachine{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.namespace},
		Spec: corev1alpha1.RouterMachineSpec{InfrastructureRef: corev1alpha1.ContractReference{
			APIGroup: infrav1alpha1.GroupVersion.Group, Kind: "HetznerMachine", Name: name,
		}},
	}
	for _, object := range []client.Object{router, machine} {
		if err := k8s.Create(f.ctx, object); err != nil {
			f.t.Fatalf("create %T: %v", object, err)
		}
	}
}

func (f *fixture) key(name string) types.NamespacedName {
	return types.NamespacedName{Namespace: f.namespace, Name: name}
}

// setReady plays the Router controller: Ready has had the given status for
// the given time.
func (f *fixture) setReady(name string, status metav1.ConditionStatus, phase corev1alpha1.RouterPhase, ago time.Duration) {
	f.t.Helper()
	router := &corev1alpha1.Router{}
	if err := k8s.Get(f.ctx, f.key(name), router); err != nil {
		f.t.Fatal(err)
	}
	router.Status.Phase = phase
	router.Status.Conditions = slices.DeleteFunc(router.Status.Conditions, func(c metav1.Condition) bool { return c.Type == corev1alpha1.ReadyCondition })
	router.Status.Conditions = append(router.Status.Conditions, metav1.Condition{
		Type: corev1alpha1.ReadyCondition, Status: status, Reason: "Test",
		ObservedGeneration: router.Generation, LastTransitionTime: metav1.NewTime(f.now.Add(-ago)),
	})
	if err := k8s.Status().Update(f.ctx, router); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) breakRouter(name string, ago time.Duration) {
	f.t.Helper()
	f.setReady(name, metav1.ConditionFalse, corev1alpha1.RouterPhaseDegraded, ago)
}

func (f *fixture) reconcile() reconcile.Result {
	f.t.Helper()
	result, err := f.r.Reconcile(f.ctx, reconcile.Request{NamespacedName: f.key("edge")})
	if err != nil {
		f.t.Fatalf("Reconcile: %v", err)
	}
	return result
}

func (f *fixture) check() *corev1alpha1.RouterHealthCheck {
	f.t.Helper()
	check := &corev1alpha1.RouterHealthCheck{}
	if err := k8s.Get(f.ctx, f.key("edge"), check); err != nil {
		f.t.Fatal(err)
	}
	return check
}

func (f *fixture) updateCheck(mutate func(*corev1alpha1.RouterHealthCheckSpec)) {
	f.t.Helper()
	check := f.check()
	mutate(&check.Spec)
	if err := k8s.Update(f.ctx, check); err != nil {
		f.t.Fatal(err)
	}
}

// request returns the remediation request on a router's machine.
func (f *fixture) request(name string) string {
	f.t.Helper()
	machine := &corev1alpha1.RouterMachine{}
	if err := k8s.Get(f.ctx, f.key(name), machine); err != nil {
		f.t.Fatal(err)
	}
	return machine.Annotations[corev1alpha1.RemediationAnnotation]
}

func (f *fixture) exists(name string) bool {
	f.t.Helper()
	router := &corev1alpha1.Router{}
	err := k8s.Get(f.ctx, f.key(name), router)
	if apierrors.IsNotFound(err) {
		return false
	}
	if err != nil {
		f.t.Fatal(err)
	}
	return router.DeletionTimestamp.IsZero()
}

func TestHealthyRoutersAreLeftAlone(t *testing.T) {
	f := newFixture(t, "a", "b")

	f.reconcile()

	status := f.check().Status
	if status.ExpectedRouters != 2 || status.CurrentHealthy != 2 {
		t.Errorf("status = %+v", status)
	}
	if !slices.Equal(status.Targets, []string{"a", "b"}) {
		t.Errorf("targets = %v", status.Targets)
	}
	if !conditions.IsTrue(status.Conditions, corev1alpha1.RemediationAllowedCondition) {
		t.Error("RemediationAllowed is not True")
	}
	if f.request("a") != "" || f.request("b") != "" {
		t.Error("a healthy router was remediated")
	}
}

func TestABlipIsNotAFailure(t *testing.T) {
	f := newFixture(t, "a", "b")
	f.breakRouter("a", time.Minute)

	result := f.reconcile()

	if f.request("a") != "" {
		t.Fatal("a router that has been unready for a minute was rebooted; the default is five")
	}
	if got := f.check().Status.CurrentHealthy; got != 2 {
		t.Errorf("currentHealthy = %d: not yet unhealthy", got)
	}
	// Nothing else will wake the controller when the five minutes are up.
	if result.RequeueAfter <= 0 || result.RequeueAfter > 4*time.Minute+time.Second {
		t.Errorf("RequeueAfter = %v, want about four minutes", result.RequeueAfter)
	}
}

func TestRebootsFirst(t *testing.T) {
	f := newFixture(t, "a", "b")
	f.breakRouter("a", 6*time.Minute)

	f.reconcile()

	request := f.request("a")
	if !strings.HasPrefix(request, "Reboot/1/") {
		t.Fatalf("request = %q, want a first reboot", request)
	}
	if !f.exists("a") {
		t.Fatal("the router was deleted without trying a reboot")
	}
	if f.request("b") != "" {
		t.Error("the healthy router was remediated")
	}
	if got := f.check().Status.CurrentHealthy; got != 1 {
		t.Errorf("currentHealthy = %d", got)
	}

	// Still within the time a reboot gets: the request is not repeated.
	f.now = f.now.Add(time.Minute)
	f.reconcile()
	if got := f.request("a"); got != request {
		t.Errorf("request changed to %q while the reboot is still in progress", got)
	}
}

func TestReplacesWhenTheRebootDidNotHelp(t *testing.T) {
	f := newFixture(t, "a", "b")
	f.breakRouter("a", 6*time.Minute)
	f.reconcile()

	f.now = f.now.Add(DefaultRebootTimeout + time.Second)
	f.reconcile()

	if f.exists("a") {
		t.Error("the router is still there after a reboot that did not help")
	}
	if !f.exists("b") {
		t.Error("the healthy router was deleted")
	}
}

func TestRecoveryResetsTheCount(t *testing.T) {
	f := newFixture(t, "a", "b")
	f.breakRouter("a", 6*time.Minute)
	f.reconcile()

	f.setReady("a", metav1.ConditionTrue, corev1alpha1.RouterPhaseReady, 0)
	f.reconcile()

	// Otherwise the next failure, a month from now, would skip the reboot.
	if got := f.request("a"); got != "" {
		t.Errorf("request = %q, want it cleared once the router recovered", got)
	}
}

func TestTooManyUnhealthyMeansHandsOff(t *testing.T) {
	f := newFixture(t, "a", "b")
	f.breakRouter("a", 6*time.Minute)
	f.breakRouter("b", 6*time.Minute)

	f.reconcile()

	// Both routers look broken at once. That is far more likely to be the
	// lab's uplink than two simultaneous failures in another data centre,
	// and rebooting everything would turn a monitoring problem into an
	// outage.
	if f.request("a") != "" || f.request("b") != "" {
		t.Error("routers were rebooted although more than maxUnhealthy are unhealthy")
	}
	condition := conditions.Get(f.check().Status.Conditions, corev1alpha1.RemediationAllowedCondition)
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != ReasonTooManyUnhealthy {
		t.Errorf("RemediationAllowed = %+v", condition)
	}
}

func TestStartupGetsItsOwnTimeout(t *testing.T) {
	f := newFixture(t, "a", "b")
	// b is new: it has not been ready yet.
	f.setReady("b", metav1.ConditionFalse, corev1alpha1.RouterPhaseProvisioning, 10*time.Minute)

	f.reconcile()
	if f.request("b") != "" {
		t.Fatal("a router that is still being created was rebooted")
	}

	f.now = f.now.Add(DefaultStartupTimeout)
	f.reconcile()
	if !strings.HasPrefix(f.request("b"), "Reboot/1/") {
		t.Errorf("request = %q: a router that never came up was not remediated", f.request("b"))
	}
}

func TestNoRebootAttempts(t *testing.T) {
	f := newFixture(t, "a", "b")
	f.updateCheck(func(spec *corev1alpha1.RouterHealthCheckSpec) { spec.Remediation.RebootAttempts = ptr.To(int32(0)) })
	f.breakRouter("a", 6*time.Minute)

	f.reconcile()

	if f.exists("a") {
		t.Error("the router was not replaced")
	}
}

func TestCustomConditionAndMaxUnhealthy(t *testing.T) {
	f := newFixture(t, "a", "b")
	f.updateCheck(func(spec *corev1alpha1.RouterHealthCheckSpec) {
		spec.UnhealthyConditions = []corev1alpha1.UnhealthyCondition{
			{Type: corev1alpha1.ReadyCondition, Status: metav1.ConditionFalse, Timeout: metav1.Duration{Duration: 30 * time.Second}},
		}
		spec.MaxUnhealthy = ptr.To(intstr.FromInt32(2))
	})
	f.breakRouter("a", time.Minute)
	f.breakRouter("b", time.Minute)

	f.reconcile()

	if !strings.HasPrefix(f.request("a"), "Reboot/1/") || !strings.HasPrefix(f.request("b"), "Reboot/1/") {
		t.Errorf("requests = %q, %q", f.request("a"), f.request("b"))
	}
}

func TestARouterNobodyWouldRecreateIsNotDeleted(t *testing.T) {
	f := newFixture(t, "a", "b")
	f.createRouter("solo", nil)
	f.breakRouter("solo", 6*time.Minute)
	f.reconcile()
	if !strings.HasPrefix(f.request("solo"), "Reboot/1/") {
		t.Fatalf("request = %q", f.request("solo"))
	}

	f.now = f.now.Add(DefaultRebootTimeout + time.Second)
	f.reconcile()

	// Deleting a router that belongs to a set is a replacement. Deleting one
	// that was created by hand is just a deletion.
	if !f.exists("solo") {
		t.Error("a router without a RouterSet was deleted")
	}
}

func TestDrainingRoutersAreSkipped(t *testing.T) {
	f := newFixture(t, "a", "b")
	router := &corev1alpha1.Router{}
	if err := k8s.Get(f.ctx, f.key("a"), router); err != nil {
		t.Fatal(err)
	}
	router.Annotations = map[string]string{corev1alpha1.DrainAnnotation: "x"}
	if err := k8s.Update(f.ctx, router); err != nil {
		t.Fatal(err)
	}
	f.breakRouter("a", 6*time.Minute)

	f.reconcile()

	if f.request("a") != "" {
		t.Error("a router that is being taken out of service was rebooted")
	}
}
