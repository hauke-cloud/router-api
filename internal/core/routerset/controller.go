// Package routerset reconciles RouterSets: it keeps a number of identical
// routers in existence and rolls configuration changes through them one at a
// time.
package routerset

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1alpha1 "github.com/hauke-cloud/router-api/api/core/v1alpha1"
	"github.com/hauke-cloud/router-api/internal/conditions"
	"github.com/hauke-cloud/router-api/internal/contract"
	"github.com/hauke-cloud/router-api/internal/pace"
)

// Condition reasons set by this controller.
const (
	ReasonPaused           = "Paused"
	ReasonNotPaused        = "NotPaused"
	ReasonAvailable        = "Available"
	ReasonNotEnoughRouters = "NotEnoughRouters"
	ReasonInvalidSelector  = "InvalidSelector"
	ReasonTemplateNotFound = "TemplateNotFound"
)

const (
	// nameSuffixLength is the length of the random part of a router's name.
	nameSuffixLength = 5
	// waitInterval is how soon to look again while routers are settling.
	waitInterval = 15 * time.Second
)

// DrainTimeout is how long a router gets to hand over before it is deleted
// regardless. A router that cannot be reached never reports being drained,
// and must not be able to block a scale-down for good.
const DrainTimeout = 3 * time.Minute

// Reconciler reconciles RouterSets.
type Reconciler struct {
	client.Client
	// Now is the clock. Defaults to time.Now.
	Now func() time.Time
}

// SetupWithManager registers the controller.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.RouterSet{}).
		Owns(&corev1alpha1.Router{}).
		Complete(r)
}

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Reconcile brings one RouterSet up to date.
func (r *Reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	set := &corev1alpha1.RouterSet{}
	if err := r.Get(ctx, req.NamespacedName, set); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if !set.DeletionTimestamp.IsZero() {
		// The routers are owned by the set; the garbage collector deletes
		// them, and each router's own finalizer does the orderly part.
		return reconcile.Result{}, nil
	}

	original := set.DeepCopy()
	result, err := r.reconcile(ctx, set)
	if !equality.Semantic.DeepEqual(set.Status, original.Status) {
		if statusErr := client.IgnoreNotFound(r.Status().Patch(ctx, set, client.MergeFrom(original))); statusErr != nil && err == nil {
			err = statusErr
		}
	}
	return result, err
}

func (r *Reconciler) reconcile(ctx context.Context, set *corev1alpha1.RouterSet) (reconcile.Result, error) {
	status := &set.Status
	status.ObservedGeneration = set.Generation

	if _, paused := set.Annotations[corev1alpha1.PausedAnnotation]; paused {
		conditions.True(&status.Conditions, set.Generation, corev1alpha1.PausedCondition, ReasonPaused, "the paused annotation is set")
		return reconcile.Result{}, nil
	}
	conditions.False(&status.Conditions, set.Generation, corev1alpha1.PausedCondition, ReasonNotPaused, "")

	selector, err := metav1.LabelSelectorAsSelector(&set.Spec.Selector)
	if err == nil && (selector.Empty() || !selector.Matches(labels.Set(set.Spec.Template.Labels))) {
		err = errors.New("the selector has to be non-empty and match the template's labels")
	}
	if err != nil {
		// Not retried: only a change to the spec fixes it.
		conditions.False(&status.Conditions, set.Generation, corev1alpha1.AvailableCondition, ReasonInvalidSelector, err.Error())
		return reconcile.Result{}, nil
	}
	status.Selector = selector.String()

	routers, err := r.routers(ctx, set, selector)
	if err != nil {
		return reconcile.Result{}, err
	}

	templates, err := r.templates(ctx, set)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return reconcile.Result{}, err
		}
		conditions.False(&status.Conditions, set.Generation, corev1alpha1.AvailableCondition, ReasonTemplateNotFound, err.Error())
		r.count(set, routers, nil)
		return reconcile.Result{RequeueAfter: pace.Every(waitInterval)}, nil
	}

	// A router whose parts are missing is finished before anything else is
	// started: creation is several writes and can stop between any two.
	for i := range routers {
		if err := r.ensureParts(ctx, set, &routers[i], templates); err != nil {
			return reconcile.Result{}, err
		}
	}

	want := int(ptr.Deref(set.Spec.Replicas, 1))
	switch {
	case len(routers) < want:
		free, err := r.freeSlots(ctx, set)
		if err != nil {
			return reconcile.Result{}, err
		}
		for range want - len(routers) {
			slot := ""
			if set.Spec.Slots != nil {
				if len(free) == 0 {
					// Every slot is held, if only by a router whose server
					// is still being deleted. Its successor has to wait:
					// what belongs to the slot is not free before then.
					break
				}
				slot, free = free[0], free[1:]
			}
			router, err := r.createRouter(ctx, set, templates, slot)
			if err != nil {
				return reconcile.Result{}, err
			}
			routers = append(routers, *router)
		}
	case len(routers) > want:
		routers, err = r.scaleDown(ctx, set, routers, len(routers)-want)
		if err != nil {
			return reconcile.Result{}, err
		}
	}

	upToDate, err := r.syncConfig(ctx, routers, templates)
	if err != nil {
		return reconcile.Result{}, err
	}

	wake := r.count(set, routers, upToDate)
	if int(status.AvailableReplicas) >= want {
		conditions.True(&status.Conditions, set.Generation, corev1alpha1.AvailableCondition, ReasonAvailable, "")
	} else {
		conditions.False(&status.Conditions, set.Generation, corev1alpha1.AvailableCondition, ReasonNotEnoughRouters,
			fmt.Sprintf("%d of %d routers are available", status.AvailableReplicas, want))
	}

	// Config objects are of kinds core cannot watch, so a set that is
	// settling is looked at again on a timer.
	settled := len(routers) == want && int(status.AvailableReplicas) == want && int(status.UpToDateReplicas) == want
	switch {
	case wake > 0:
		return reconcile.Result{RequeueAfter: wake}, nil
	case !settled:
		return reconcile.Result{RequeueAfter: pace.Every(waitInterval)}, nil
	}
	return reconcile.Result{RequeueAfter: pace.Every(4 * waitInterval)}, nil
}

// routers lists the routers of the set that are not being deleted, oldest
// first.
func (r *Reconciler) routers(ctx context.Context, set *corev1alpha1.RouterSet, selector labels.Selector) ([]corev1alpha1.Router, error) {
	list := &corev1alpha1.RouterList{}
	if err := r.List(ctx, list, client.InNamespace(set.Namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return nil, err
	}
	routers := slices.DeleteFunc(list.Items, func(router corev1alpha1.Router) bool {
		return !router.DeletionTimestamp.IsZero() || !metav1.IsControlledBy(&router, set)
	})
	slices.SortFunc(routers, func(a, b corev1alpha1.Router) int {
		if c := a.CreationTimestamp.Compare(b.CreationTimestamp.Time); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	return routers, nil
}

type templates struct {
	infra, config *unstructured.Unstructured
	configSpec    map[string]any
}

func (r *Reconciler) templates(ctx context.Context, set *corev1alpha1.RouterSet) (*templates, error) {
	infra, err := contract.Get(ctx, r.Client, set.Namespace, set.Spec.Template.Spec.InfrastructureTemplateRef)
	if err != nil {
		return nil, err
	}
	config, err := contract.Get(ctx, r.Client, set.Namespace, set.Spec.Template.Spec.ConfigTemplateRef)
	if err != nil {
		return nil, err
	}
	configSpec, err := contract.TemplateSpec(config)
	if err != nil {
		return nil, err
	}
	return &templates{infra: infra, config: config, configSpec: configSpec}, nil
}

func (r *Reconciler) routerLabels(set *corev1alpha1.RouterSet, name, slot string) map[string]string {
	out := map[string]string{}
	for k, v := range set.Spec.Template.Labels {
		out[k] = v
	}
	// Carried over from the set so that the routers of one deployment find
	// each other across the sets of a rollout.
	for _, key := range []string{corev1alpha1.DeploymentNameLabel, corev1alpha1.TemplateHashLabel} {
		if value, ok := set.Labels[key]; ok {
			out[key] = value
		}
	}
	out[corev1alpha1.SetNameLabel] = set.Name
	out[corev1alpha1.RouterNameLabel] = name
	if slot != "" {
		out[corev1alpha1.SlotLabel] = slot
	}
	return out
}

// createRouter creates a router and its parts. slot is empty for a set
// without slots.
func (r *Reconciler) createRouter(ctx context.Context, set *corev1alpha1.RouterSet, t *templates, slot string) (*corev1alpha1.Router, error) {
	// The name is new every time, also for the successor in a slot: the
	// Secrets of a router are named after it, and those of its predecessor
	// are still on their way out when it is created.
	name := set.Name + "-" + rand.String(nameSuffixLength)
	if slot != "" {
		name = set.Name + "-" + slot + "-" + rand.String(nameSuffixLength)
	}
	router := &corev1alpha1.Router{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   set.Namespace,
			Labels:      r.routerLabels(set, name, slot),
			Annotations: set.Spec.Template.Annotations,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(set, corev1alpha1.GroupVersion.WithKind("RouterSet")),
			},
		},
		Spec: corev1alpha1.RouterSpec{
			MachineRef: corev1alpha1.LocalObjectReference{Name: name},
			ConfigRef: corev1alpha1.ContractReference{
				APIGroup: t.config.GroupVersionKind().Group,
				Kind:     strings.TrimSuffix(t.config.GetKind(), contract.TemplateSuffix),
				Name:     name,
			},
		},
	}
	if err := r.Create(ctx, router); err != nil {
		return nil, fmt.Errorf("create Router: %w", err)
	}
	if err := r.ensureParts(ctx, set, router, t); err != nil {
		return nil, err
	}
	return router, nil
}

// ensureParts creates whichever of a router's config, machine and
// infrastructure object does not exist. All three carry the router's name.
func (r *Reconciler) ensureParts(ctx context.Context, set *corev1alpha1.RouterSet, router *corev1alpha1.Router, t *templates) error {
	routerLabels := r.routerLabels(set, router.Name, router.Labels[corev1alpha1.SlotLabel])
	routerOwner := metav1.NewControllerRef(router, corev1alpha1.GroupVersion.WithKind("Router"))

	config, err := contract.FromTemplate(t.config, router.Name, routerLabels, routerOwner)
	if err != nil {
		return err
	}
	if err := r.createIfMissing(ctx, config); err != nil {
		return err
	}

	machine := &corev1alpha1.RouterMachine{}
	err = r.Get(ctx, types.NamespacedName{Namespace: router.Namespace, Name: router.Spec.MachineRef.Name}, machine)
	if apierrors.IsNotFound(err) {
		machine = &corev1alpha1.RouterMachine{
			ObjectMeta: metav1.ObjectMeta{
				Name:            router.Spec.MachineRef.Name,
				Namespace:       router.Namespace,
				Labels:          routerLabels,
				OwnerReferences: []metav1.OwnerReference{*routerOwner},
			},
			Spec: corev1alpha1.RouterMachineSpec{
				InfrastructureRef: corev1alpha1.ContractReference{
					APIGroup: t.infra.GroupVersionKind().Group,
					Kind:     strings.TrimSuffix(t.infra.GetKind(), contract.TemplateSuffix),
					Name:     router.Name,
				},
			},
		}
		err = r.Create(ctx, machine)
	}
	if err != nil {
		return fmt.Errorf("RouterMachine %s: %w", router.Spec.MachineRef.Name, err)
	}
	if !machine.DeletionTimestamp.IsZero() {
		// On its way out, and its server with it. Nothing to add to that.
		return nil
	}

	// The infrastructure object is owned by the machine, so that the machine
	// controller finds it adopted and the server goes when the machine goes.
	infra, err := contract.FromTemplate(t.infra, machine.Spec.InfrastructureRef.Name, routerLabels,
		metav1.NewControllerRef(machine, corev1alpha1.GroupVersion.WithKind("RouterMachine")))
	if err != nil {
		return err
	}
	return r.createIfMissing(ctx, infra)
}

func (r *Reconciler) createIfMissing(ctx context.Context, object *unstructured.Unstructured) error {
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(object.GroupVersionKind())
	err := r.Get(ctx, client.ObjectKeyFromObject(object), existing)
	if apierrors.IsNotFound(err) {
		err = r.Create(ctx, object)
	}
	if err != nil {
		return fmt.Errorf("%s %s: %w", object.GetKind(), object.GetName(), err)
	}
	return nil
}

// slotOf returns the slot of a router, or -1 if it has none.
func slotOf(router *corev1alpha1.Router) int {
	slot, err := strconv.Atoi(router.Labels[corev1alpha1.SlotLabel])
	if err != nil || slot < 0 {
		return -1
	}
	return slot
}

// outOfRange reports whether a router sits in a slot the group no longer has,
// which happens when the group is made smaller.
func outOfRange(set *corev1alpha1.RouterSet, router *corev1alpha1.Router) bool {
	return set.Spec.Slots != nil && slotOf(router) >= int(*set.Spec.Slots)
}

// freeSlots returns the slots no router of the group holds, lowest first, or
// nil for a set without slots.
//
// A slot is held by any Router object that exists, whichever set of the group
// it belongs to and whether or not it is being deleted. A router that is
// being deleted still has its server, and with it whatever is tied to the
// slot.
func (r *Reconciler) freeSlots(ctx context.Context, set *corev1alpha1.RouterSet) ([]string, error) {
	if set.Spec.Slots == nil {
		return nil, nil
	}
	// The group is the deployment; a set on its own is its own group.
	group := client.MatchingLabels{corev1alpha1.SetNameLabel: set.Name}
	if deployment, ok := set.Labels[corev1alpha1.DeploymentNameLabel]; ok {
		group = client.MatchingLabels{corev1alpha1.DeploymentNameLabel: deployment}
	}
	list := &corev1alpha1.RouterList{}
	if err := r.List(ctx, list, client.InNamespace(set.Namespace), group); err != nil {
		return nil, err
	}
	held := map[int]bool{}
	for i := range list.Items {
		held[slotOf(&list.Items[i])] = true
	}
	var free []string
	for slot := range int(*set.Spec.Slots) {
		if !held[slot] {
			free = append(free, strconv.Itoa(slot))
		}
	}
	return free, nil
}

func isReady(router *corev1alpha1.Router) bool {
	return conditions.IsTrue(router.Status.Conditions, corev1alpha1.ReadyCondition)
}

// isActive reports whether a router is the one carrying the traffic, as far
// as its config provider can tell.
func isActive(router *corev1alpha1.Router) bool {
	return conditions.IsTrue(router.Status.Conditions, corev1alpha1.ActiveCondition)
}

// isSettled reports whether a router is in service and runs the configuration
// its config object describes: nothing is in flight on it.
//
// Whether the configuration is applied is read from the config object itself,
// for the generation it has now. The Router mirrors that condition, but only
// when it is next reconciled, and in between it still shows the verdict on
// the configuration from before. Going by the mirror, a router that was
// handed a change a moment ago looks as if it had already applied it, and
// the change would be handed to the next router straight away.
func isSettled(router *corev1alpha1.Router, config *unstructured.Unstructured) bool {
	return isReady(router) && config != nil && contract.IsConditionTrue(config, corev1alpha1.ConfigAppliedCondition)
}

// scaleDown removes up to excess routers and returns the ones that remain.
// Routers that do not work go at once. A working one is first asked to hand
// its addresses to a peer, one router at a time.
func (r *Reconciler) scaleDown(ctx context.Context, set *corev1alpha1.RouterSet, routers []corev1alpha1.Router, excess int) ([]corev1alpha1.Router, error) {
	// Least valuable first: in a slot the group no longer has, broken,
	// already draining, standby, then newest. The standby before the active
	// router, because taking the active one first costs a failover, and its
	// successor's turn then costs a second.
	victims := slices.Clone(routers)
	slices.SortStableFunc(victims, func(a, b corev1alpha1.Router) int {
		if aOut, bOut := outOfRange(set, &a), outOfRange(set, &b); aOut != bOut {
			if aOut {
				return -1
			}
			return 1
		}
		if isReady(&a) != isReady(&b) {
			if isReady(&a) {
				return 1
			}
			return -1
		}
		_, aDraining := a.Annotations[corev1alpha1.DrainAnnotation]
		_, bDraining := b.Annotations[corev1alpha1.DrainAnnotation]
		if aDraining != bDraining {
			if aDraining {
				return -1
			}
			return 1
		}
		if isActive(&a) != isActive(&b) {
			if isActive(&a) {
				return 1
			}
			return -1
		}
		return b.CreationTimestamp.Compare(a.CreationTimestamp.Time)
	})

	deleted := map[string]bool{}
	draining := false
	for i := range victims[:excess] {
		victim := &victims[i]
		if isReady(victim) {
			if draining {
				// One hand-over at a time.
				continue
			}
			draining = true
			done, err := r.drain(ctx, victim)
			if err != nil {
				return nil, err
			}
			if !done {
				continue
			}
		}
		if err := client.IgnoreNotFound(r.Delete(ctx, victim)); err != nil {
			return nil, fmt.Errorf("delete Router %s: %w", victim.Name, err)
		}
		deleted[victim.Name] = true
	}
	return slices.DeleteFunc(routers, func(router corev1alpha1.Router) bool { return deleted[router.Name] }), nil
}

// drain asks a router to hand over and reports whether it has, or has had
// long enough.
func (r *Reconciler) drain(ctx context.Context, router *corev1alpha1.Router) (bool, error) {
	requested, ok := router.Annotations[corev1alpha1.DrainAnnotation]
	if !ok {
		original := router.DeepCopy()
		if router.Annotations == nil {
			router.Annotations = map[string]string{}
		}
		router.Annotations[corev1alpha1.DrainAnnotation] = r.now().UTC().Format(time.RFC3339)
		return false, r.Patch(ctx, router, client.MergeFrom(original))
	}
	if conditions.IsTrue(router.Status.Conditions, corev1alpha1.DrainedCondition) {
		return true, nil
	}
	since, err := time.Parse(time.RFC3339, requested)
	if err != nil {
		// Asked to drain by someone else, without a time to count from.
		since = router.CreationTimestamp.Time
	}
	return r.now().Sub(since) > DrainTimeout, nil
}

// syncConfig copies the config template's spec to the routers' config
// objects, at most one router per call, and returns the names of the routers
// whose config matches the template.
//
// A working router is only changed while every other router is settled: if
// the change breaks it, the others still carry the traffic. A router that is
// already out of service is changed first, since it has nothing to lose and
// the change may be the fix; then the standby, and the active router last. And while a router that already has the new
// configuration has not applied it, nothing further is changed: either it is
// still at it, or it refused, and then the change itself is the first
// suspect.
//
// A router counts as up to date once it has the configuration and runs it.
func (r *Reconciler) syncConfig(ctx context.Context, routers []corev1alpha1.Router, t *templates) (map[string]bool, error) {
	upToDate := map[string]bool{}
	hasSpec := map[string]bool{}
	configs := map[string]*unstructured.Unstructured{}
	for i := range routers {
		router := &routers[i]
		config, err := contract.Get(ctx, r.Client, router.Namespace, router.Spec.ConfigRef)
		if err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, err
		}
		configs[router.Name] = config
		current, _, _ := unstructured.NestedMap(config.Object, "spec")
		hasSpec[router.Name] = equality.Semantic.DeepEqual(current, t.configSpec)
		upToDate[router.Name] = hasSpec[router.Name] && isSettled(router, config)
	}

	var candidate *corev1alpha1.Router
	for i := range routers {
		router := &routers[i]
		switch {
		case configs[router.Name] == nil:
			continue
		case hasSpec[router.Name] && !isSettled(router, configs[router.Name]):
			return upToDate, nil
		case !hasSpec[router.Name] && (candidate == nil || changeRank(router) < changeRank(candidate)):
			candidate = router
		}
	}
	if candidate == nil {
		return upToDate, nil
	}
	if isReady(candidate) {
		for i := range routers {
			if routers[i].Name != candidate.Name && !isSettled(&routers[i], configs[routers[i].Name]) {
				return upToDate, nil
			}
		}
	}

	config := configs[candidate.Name]
	if _, err := contract.SetSpec(config, t.configSpec); err != nil {
		return nil, err
	}
	if err := r.Update(ctx, config); err != nil {
		return nil, fmt.Errorf("update %s %s: %w", config.GetKind(), config.GetName(), err)
	}
	// Not up to date yet: it has the configuration, it does not run it.
	return upToDate, nil
}

// changeRank orders routers by how little is lost if a configuration change
// goes wrong on them: one that is out of service anyway, then the standby,
// then the router that carries the traffic.
func changeRank(router *corev1alpha1.Router) int {
	switch {
	case !isReady(router):
		return 0
	case !isActive(router):
		return 1
	}
	return 2
}

// count fills in the replica counters and returns how long until a router
// that is ready becomes available, or 0 if none is waiting for that.
func (r *Reconciler) count(set *corev1alpha1.RouterSet, routers []corev1alpha1.Router, upToDate map[string]bool) time.Duration {
	status := &set.Status
	status.Replicas = int32(len(routers)) //nolint:gosec // bounded by spec.replicas
	status.ReadyReplicas, status.AvailableReplicas, status.UpToDateReplicas = 0, 0, 0

	minReady := time.Duration(set.Spec.MinReadySeconds) * time.Second
	var wake time.Duration
	for i := range routers {
		router := &routers[i]
		if upToDate[router.Name] {
			status.UpToDateReplicas++
		}
		ready := conditions.Get(router.Status.Conditions, corev1alpha1.ReadyCondition)
		if ready == nil || ready.Status != metav1.ConditionTrue {
			continue
		}
		status.ReadyReplicas++
		remaining := minReady - r.now().Sub(ready.LastTransitionTime.Time)
		if remaining <= 0 {
			status.AvailableReplicas++
		} else if wake == 0 || remaining < wake {
			wake = remaining
		}
	}
	return wake
}
