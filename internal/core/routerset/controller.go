// Package routerset reconciles RouterSets: it keeps a number of identical
// routers in existence and rolls configuration changes through them one at a
// time.
package routerset

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
		return reconcile.Result{RequeueAfter: waitInterval}, nil
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
		for range want - len(routers) {
			router, err := r.createRouter(ctx, set, templates)
			if err != nil {
				return reconcile.Result{}, err
			}
			routers = append(routers, *router)
		}
	case len(routers) > want:
		routers, err = r.scaleDown(ctx, routers, len(routers)-want)
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
		return reconcile.Result{RequeueAfter: waitInterval}, nil
	}
	return reconcile.Result{RequeueAfter: 4 * waitInterval}, nil
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

func (r *Reconciler) routerLabels(set *corev1alpha1.RouterSet, name string) map[string]string {
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
	return out
}

func (r *Reconciler) createRouter(ctx context.Context, set *corev1alpha1.RouterSet, t *templates) (*corev1alpha1.Router, error) {
	name := set.Name + "-" + rand.String(nameSuffixLength)
	router := &corev1alpha1.Router{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   set.Namespace,
			Labels:      r.routerLabels(set, name),
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
	routerLabels := r.routerLabels(set, router.Name)
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

func isReady(router *corev1alpha1.Router) bool {
	return conditions.IsTrue(router.Status.Conditions, corev1alpha1.ReadyCondition)
}

// scaleDown removes up to excess routers and returns the ones that remain.
// Routers that do not work go at once. A working one is first asked to hand
// its addresses to a peer, one router at a time.
func (r *Reconciler) scaleDown(ctx context.Context, routers []corev1alpha1.Router, excess int) ([]corev1alpha1.Router, error) {
	// Least valuable first: broken, then already draining, then newest.
	victims := slices.Clone(routers)
	slices.SortStableFunc(victims, func(a, b corev1alpha1.Router) int {
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
// A working router is only changed while every other router works: if the
// change breaks it, the others still carry the traffic. A router that is
// already broken is changed first, since it has nothing to lose and the
// change may be the fix. And when a router that already has the new
// configuration is not ready, nothing further is changed: the change itself
// is the first suspect.
func (r *Reconciler) syncConfig(ctx context.Context, routers []corev1alpha1.Router, t *templates) (map[string]bool, error) {
	upToDate := map[string]bool{}
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
		upToDate[router.Name] = equality.Semantic.DeepEqual(current, t.configSpec)
	}

	var candidate *corev1alpha1.Router
	for i := range routers {
		router := &routers[i]
		switch {
		case configs[router.Name] == nil:
			continue
		case upToDate[router.Name] && !isReady(router):
			return upToDate, nil
		case !upToDate[router.Name] && (candidate == nil || (isReady(candidate) && !isReady(router))):
			candidate = router
		}
	}
	if candidate == nil {
		return upToDate, nil
	}
	if isReady(candidate) {
		for i := range routers {
			if routers[i].Name != candidate.Name && !isReady(&routers[i]) {
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
	upToDate[candidate.Name] = true
	return upToDate, nil
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
