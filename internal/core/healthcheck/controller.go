// Package healthcheck reconciles RouterHealthChecks: it reboots, and failing
// that replaces, routers that stopped working, unless so many look broken at
// once that the observer is the likelier culprit.
package healthcheck

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1alpha1 "github.com/hauke-cloud/router-api/api/core/v1alpha1"
	"github.com/hauke-cloud/router-api/internal/conditions"
	"github.com/hauke-cloud/router-api/internal/pace"
)

// Condition reasons set by this controller.
const (
	ReasonRemediationAllowed = "RemediationAllowed"
	ReasonTooManyUnhealthy   = "TooManyUnhealthy"
	ReasonInvalidSelector    = "InvalidSelector"
)

// Defaults for fields the API server normally fills in.
const (
	DefaultUnhealthyTimeout = 5 * time.Minute
	DefaultStartupTimeout   = 15 * time.Minute
	DefaultRebootTimeout    = 5 * time.Minute
	defaultMaxUnhealthy     = "50%"
	defaultRebootAttempts   = 1
	// waitInterval is how often a check is repeated when nothing is about
	// to time out.
	waitInterval = 30 * time.Second
)

// Reconciler reconciles RouterHealthChecks.
type Reconciler struct {
	client.Client
	// Now is the clock. Defaults to time.Now.
	Now func() time.Time
}

// SetupWithManager registers the controller. A change to any Router wakes
// every health check in its namespace; there are few of either.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.RouterHealthCheck{}).
		Watches(&corev1alpha1.Router{}, handler.EnqueueRequestsFromMapFunc(r.checksFor)).
		Complete(r)
}

func (r *Reconciler) checksFor(ctx context.Context, object client.Object) []reconcile.Request {
	list := &corev1alpha1.RouterHealthCheckList{}
	if err := r.List(ctx, list, client.InNamespace(object.GetNamespace())); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, len(list.Items))
	for i := range list.Items {
		requests[i] = reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])}
	}
	return requests
}

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Reconcile checks the routers of one RouterHealthCheck.
func (r *Reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	check := &corev1alpha1.RouterHealthCheck{}
	if err := r.Get(ctx, req.NamespacedName, check); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if !check.DeletionTimestamp.IsZero() {
		return reconcile.Result{}, nil
	}

	original := check.DeepCopy()
	result, err := r.reconcile(ctx, check)
	if !equality.Semantic.DeepEqual(check.Status, original.Status) {
		if statusErr := client.IgnoreNotFound(r.Status().Patch(ctx, check, client.MergeFrom(original))); statusErr != nil && err == nil {
			err = statusErr
		}
	}
	return result, err
}

func (r *Reconciler) reconcile(ctx context.Context, check *corev1alpha1.RouterHealthCheck) (reconcile.Result, error) {
	status := &check.Status
	status.ObservedGeneration = check.Generation

	selector, selectorErr := metav1.LabelSelectorAsSelector(&check.Spec.Selector)
	if selectorErr != nil || selector.Empty() {
		// Only an edit of the spec fixes this, so it is reported and not
		// retried.
		conditions.False(&status.Conditions, check.Generation, corev1alpha1.RemediationAllowedCondition, ReasonInvalidSelector,
			"the selector has to be valid and non-empty: a health check must not select every router by accident")
		return reconcile.Result{}, nil //nolint:nilerr // reported as a condition
	}
	list := &corev1alpha1.RouterList{}
	if err := r.List(ctx, list, client.InNamespace(check.Namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return reconcile.Result{}, err
	}
	routers := slices.DeleteFunc(list.Items, func(router corev1alpha1.Router) bool { return !router.DeletionTimestamp.IsZero() })

	var (
		unhealthy []*corev1alpha1.Router
		healthy   []*corev1alpha1.Router
		wake      time.Duration
	)
	status.Targets = make([]string, 0, len(routers))
	for i := range routers {
		router := &routers[i]
		status.Targets = append(status.Targets, router.Name)
		broken, in := r.unhealthy(check, router)
		if broken {
			unhealthy = append(unhealthy, router)
			continue
		}
		healthy = append(healthy, router)
		if in > 0 && (wake == 0 || in < wake) {
			wake = in
		}
	}
	slices.Sort(status.Targets)

	expected := len(routers)
	limit, err := intstr.GetScaledValueFromIntOrPercent(
		ptr.To(ptr.Deref(check.Spec.MaxUnhealthy, intstr.FromString(defaultMaxUnhealthy))), expected, false)
	if err != nil {
		limit = 0
	}
	status.ExpectedRouters = int32(expected)                         //nolint:gosec // a count of routers
	status.CurrentHealthy = int32(len(healthy))                      //nolint:gosec // a count of routers
	status.RemediationsAllowed = int32(max(0, limit-len(unhealthy))) //nolint:gosec // a count of routers

	// A router that works again starts from zero the next time it fails.
	for _, router := range healthy {
		if err := r.clearRequest(ctx, router); err != nil {
			return reconcile.Result{}, err
		}
	}

	if len(unhealthy) > limit {
		conditions.False(&status.Conditions, check.Generation, corev1alpha1.RemediationAllowedCondition, ReasonTooManyUnhealthy,
			fmt.Sprintf("%d of %d routers are unhealthy, more than the %d that may be remediated at once; nothing is touched",
				len(unhealthy), expected, limit))
		return reconcile.Result{RequeueAfter: pace.Every(waitInterval)}, nil
	}
	conditions.True(&status.Conditions, check.Generation, corev1alpha1.RemediationAllowedCondition, ReasonRemediationAllowed, "")

	for _, router := range unhealthy {
		in, err := r.remediate(ctx, check, router)
		if err != nil {
			return reconcile.Result{}, err
		}
		if in > 0 && (wake == 0 || in < wake) {
			wake = in
		}
	}

	if wake == 0 {
		// Nothing is about to time out. Look again anyway: nothing else
		// wakes this controller when the clock is all that changes.
		wake = pace.Every(waitInterval)
	}
	return reconcile.Result{RequeueAfter: wake}, nil
}

// unhealthy reports whether a router counts as broken. If it does not but
// will once a timeout runs out, in is how long until then.
func (r *Reconciler) unhealthy(check *corev1alpha1.RouterHealthCheck, router *corev1alpha1.Router) (broken bool, in time.Duration) {
	if _, draining := router.Annotations[corev1alpha1.DrainAnnotation]; draining {
		// Being taken out of service on purpose.
		return false, 0
	}
	if _, paused := router.Annotations[corev1alpha1.PausedAnnotation]; paused {
		return false, 0
	}
	now := r.now()

	switch router.Status.Phase {
	case "", corev1alpha1.RouterPhasePending, corev1alpha1.RouterPhaseProvisioning, corev1alpha1.RouterPhaseConfiguring:
		// Has not worked yet. Creating a server and configuring it takes
		// longer than a working router is allowed to be away.
		startup := ptr.Deref(check.Spec.StartupTimeout, metav1.Duration{Duration: DefaultStartupTimeout}).Duration
		if age := now.Sub(router.CreationTimestamp.Time); age < startup {
			return false, startup - age
		}
		return true, 0
	}

	rules := check.Spec.UnhealthyConditions
	if len(rules) == 0 {
		rules = []corev1alpha1.UnhealthyCondition{
			{Type: corev1alpha1.ReadyCondition, Status: metav1.ConditionFalse, Timeout: metav1.Duration{Duration: DefaultUnhealthyTimeout}},
			{Type: corev1alpha1.ReadyCondition, Status: metav1.ConditionUnknown, Timeout: metav1.Duration{Duration: DefaultUnhealthyTimeout}},
		}
	}
	for _, rule := range rules {
		condition := conditions.Get(router.Status.Conditions, rule.Type)
		current, since := metav1.ConditionUnknown, router.CreationTimestamp.Time
		if condition != nil {
			current, since = condition.Status, condition.LastTransitionTime.Time
		}
		if current != rule.Status {
			continue
		}
		held := now.Sub(since)
		if held >= rule.Timeout.Duration {
			return true, 0
		}
		if remaining := rule.Timeout.Duration - held; in == 0 || remaining < in {
			in = remaining
		}
	}
	return false, in
}

// request is a remediation request as stored in the annotation:
// <action>/<attempt>/<time>.
type request struct {
	attempt int
	at      time.Time
}

func parseRequest(value string) (request, bool) {
	parts := strings.SplitN(value, "/", 3)
	if len(parts) != 3 || parts[0] != string(corev1alpha1.RemediationReboot) {
		return request{}, false
	}
	attempt, err := strconv.Atoi(parts[1])
	if err != nil {
		return request{}, false
	}
	at, err := time.Parse(time.RFC3339, parts[2])
	if err != nil {
		return request{}, false
	}
	return request{attempt: attempt, at: at}, true
}

// remediate takes the next step for a broken router: another reboot while
// there are attempts left, then replacement. in is how long until the step
// in progress has had its time.
func (r *Reconciler) remediate(ctx context.Context, check *corev1alpha1.RouterHealthCheck, router *corev1alpha1.Router) (in time.Duration, err error) {
	machine := &corev1alpha1.RouterMachine{}
	err = r.Get(ctx, types.NamespacedName{Namespace: router.Namespace, Name: router.Spec.MachineRef.Name}, machine)
	if err != nil && !apierrors.IsNotFound(err) {
		return 0, err
	}
	hasMachine := err == nil

	attempts := int(ptr.Deref(check.Spec.Remediation.RebootAttempts, defaultRebootAttempts))
	rebootTimeout := ptr.Deref(check.Spec.Remediation.RebootTimeout, metav1.Duration{Duration: DefaultRebootTimeout}).Duration
	now := r.now()

	last, pending := request{}, false
	if hasMachine {
		last, pending = parseRequest(machine.Annotations[corev1alpha1.RemediationAnnotation])
	}
	if pending {
		if waited := now.Sub(last.at); waited < rebootTimeout {
			return rebootTimeout - waited, nil
		}
	}

	if hasMachine && last.attempt < attempts {
		original := machine.DeepCopy()
		if machine.Annotations == nil {
			machine.Annotations = map[string]string{}
		}
		machine.Annotations[corev1alpha1.RemediationAnnotation] = fmt.Sprintf("%s/%d/%s",
			corev1alpha1.RemediationReboot, last.attempt+1, now.UTC().Format(time.RFC3339))
		if err := r.Patch(ctx, machine, client.MergeFrom(original)); err != nil {
			return 0, fmt.Errorf("request reboot of %s: %w", machine.Name, err)
		}
		return rebootTimeout, nil
	}

	// Out of reboots. Deleting the router makes its RouterSet build a new
	// one; without a set, deleting would only make the router disappear.
	owner := metav1.GetControllerOf(router)
	if owner == nil || owner.Kind != "RouterSet" {
		return 0, nil
	}
	if err := client.IgnoreNotFound(r.Delete(ctx, router)); err != nil {
		return 0, fmt.Errorf("delete Router %s: %w", router.Name, err)
	}
	return 0, nil
}

func (r *Reconciler) clearRequest(ctx context.Context, router *corev1alpha1.Router) error {
	if router.Status.Phase != corev1alpha1.RouterPhaseReady {
		// Not broken is not the same as recovered: a router that is merely
		// inside a timeout keeps its count.
		return nil
	}
	machine := &corev1alpha1.RouterMachine{}
	err := r.Get(ctx, types.NamespacedName{Namespace: router.Namespace, Name: router.Spec.MachineRef.Name}, machine)
	if err != nil {
		return client.IgnoreNotFound(err)
	}
	if _, ok := machine.Annotations[corev1alpha1.RemediationAnnotation]; !ok {
		return nil
	}
	original := machine.DeepCopy()
	delete(machine.Annotations, corev1alpha1.RemediationAnnotation)
	return r.Patch(ctx, machine, client.MergeFrom(original))
}
