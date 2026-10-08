// Package routerdeployment reconciles RouterDeployments: it replaces the
// routers of a group with new ones without the group ever being short.
package routerdeployment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
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
	ReasonPaused             = "Paused"
	ReasonNotPaused          = "NotPaused"
	ReasonAvailable          = "Available"
	ReasonNotEnoughRouters   = "NotEnoughRouters"
	ReasonInvalidSelector    = "InvalidSelector"
	ReasonTemplateNotFound   = "TemplateNotFound"
	ReasonWaitingForProvider = "WaitingForProvider"
	ReasonRollingOut         = "RollingOut"
	ReasonRolledOut          = "RolledOut"
)

const (
	// hashLength is how much of the template hash goes into names and labels.
	hashLength = 10
	// waitInterval is how soon to look again while a rollout is in flight or
	// a template is awaited. Templates are of kinds core cannot watch.
	waitInterval = 15 * time.Second
)

// Reconciler reconciles RouterDeployments.
type Reconciler struct {
	client.Client
}

// SetupWithManager registers the controller.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.RouterDeployment{}).
		Owns(&corev1alpha1.RouterSet{}).
		Complete(r)
}

// Reconcile brings one RouterDeployment up to date.
func (r *Reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	deployment := &corev1alpha1.RouterDeployment{}
	if err := r.Get(ctx, req.NamespacedName, deployment); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if !deployment.DeletionTimestamp.IsZero() {
		// The sets are owned by the deployment and go with it.
		return reconcile.Result{}, nil
	}

	original := deployment.DeepCopy()
	result, err := r.reconcile(ctx, deployment)
	if !equality.Semantic.DeepEqual(deployment.Status, original.Status) {
		if statusErr := client.IgnoreNotFound(r.Status().Patch(ctx, deployment, client.MergeFrom(original))); statusErr != nil && err == nil {
			err = statusErr
		}
	}
	return result, err
}

func (r *Reconciler) reconcile(ctx context.Context, deployment *corev1alpha1.RouterDeployment) (reconcile.Result, error) {
	status := &deployment.Status
	generation := deployment.Generation
	status.ObservedGeneration = generation

	selector, err := metav1.LabelSelectorAsSelector(&deployment.Spec.Selector)
	if err == nil && (selector.Empty() || !selector.Matches(labels.Set(deployment.Spec.Template.Labels))) {
		err = errors.New("the selector has to be non-empty and match the template's labels")
	}
	if err != nil {
		conditions.False(&status.Conditions, generation, corev1alpha1.AvailableCondition, ReasonInvalidSelector, err.Error())
		return reconcile.Result{}, nil
	}
	status.Selector = selector.String()

	sets, err := r.sets(ctx, deployment)
	if err != nil {
		return reconcile.Result{}, err
	}

	_, annotated := deployment.Annotations[corev1alpha1.PausedAnnotation]
	if deployment.Spec.Paused || annotated {
		conditions.True(&status.Conditions, generation, corev1alpha1.PausedCondition, ReasonPaused, "rollouts are paused")
		r.summarize(deployment, sets, "")
		return reconcile.Result{}, nil
	}
	conditions.False(&status.Conditions, generation, corev1alpha1.PausedCondition, ReasonNotPaused, "")

	hash, reason, err := r.templateHash(ctx, deployment)
	if err != nil {
		return reconcile.Result{}, err
	}
	if hash == "" {
		// Nothing to roll towards yet. What exists keeps running.
		r.summarize(deployment, sets, "")
		conditions.False(&status.Conditions, generation, corev1alpha1.AvailableCondition, reason.reason, reason.message)
		return reconcile.Result{RequeueAfter: waitInterval}, nil
	}

	current, sets, err := r.currentSet(ctx, deployment, sets, hash)
	if err != nil {
		return reconcile.Result{}, err
	}
	if err := r.roll(ctx, deployment, current, sets); err != nil {
		return reconcile.Result{}, err
	}
	if err := r.trimHistory(ctx, deployment, current, sets); err != nil {
		return reconcile.Result{}, err
	}

	if r.summarize(deployment, sets, current.Name) {
		return reconcile.Result{RequeueAfter: waitInterval}, nil
	}
	return reconcile.Result{RequeueAfter: 4 * waitInterval}, nil
}

// sets lists the RouterSets of the deployment, oldest revision first.
func (r *Reconciler) sets(ctx context.Context, deployment *corev1alpha1.RouterDeployment) ([]*corev1alpha1.RouterSet, error) {
	list := &corev1alpha1.RouterSetList{}
	if err := r.List(ctx, list, client.InNamespace(deployment.Namespace),
		client.MatchingLabels{corev1alpha1.DeploymentNameLabel: deployment.Name}); err != nil {
		return nil, err
	}
	var sets []*corev1alpha1.RouterSet
	for i := range list.Items {
		set := &list.Items[i]
		if metav1.IsControlledBy(set, deployment) && set.DeletionTimestamp.IsZero() {
			sets = append(sets, set)
		}
	}
	slices.SortFunc(sets, func(a, b *corev1alpha1.RouterSet) int { return revision(a) - revision(b) })
	return sets, nil
}

func revision(set *corev1alpha1.RouterSet) int {
	n, _ := strconv.Atoi(set.Annotations[corev1alpha1.RevisionAnnotation])
	return n
}

type waiting struct{ reason, message string }

// templateHash identifies everything about the deployment's template that a
// running router cannot change: a different hash means different routers.
//
// It covers the whole infrastructure template, and of the config template the
// replacement hash its provider publishes. The rest of the config template is
// deliberately left out; the RouterSet applies that to the running routers.
//
// An empty hash means it cannot be computed yet, and waiting says why.
func (r *Reconciler) templateHash(ctx context.Context, deployment *corev1alpha1.RouterDeployment) (string, waiting, error) {
	refs := deployment.Spec.Template.Spec

	infra, err := contract.Get(ctx, r.Client, deployment.Namespace, refs.InfrastructureTemplateRef)
	if apierrors.IsNotFound(err) {
		return "", waiting{ReasonTemplateNotFound, fmt.Sprintf("%s %s does not exist", refs.InfrastructureTemplateRef.Kind, refs.InfrastructureTemplateRef.Name)}, nil
	} else if err != nil {
		return "", waiting{}, err
	}
	infraSpec, specErr := contract.TemplateSpec(infra)
	if specErr != nil {
		// Not an error to retry on: the object is there and is not a
		// template. Only an edit changes that.
		return "", waiting{ReasonTemplateNotFound, specErr.Error()}, nil //nolint:nilerr // reported as a condition
	}

	config, err := contract.Get(ctx, r.Client, deployment.Namespace, refs.ConfigTemplateRef)
	if apierrors.IsNotFound(err) {
		return "", waiting{ReasonTemplateNotFound, fmt.Sprintf("%s %s does not exist", refs.ConfigTemplateRef.Kind, refs.ConfigTemplateRef.Name)}, nil
	} else if err != nil {
		return "", waiting{}, err
	}
	replacementHash := contract.ReplacementHash(config)
	observed, _, _ := contract.NestedInt64(config, "status", "observedGeneration")
	if replacementHash == "" || observed < config.GetGeneration() {
		// Rolling on a hash that describes an older template would start a
		// replacement that the next reconcile takes back.
		return "", waiting{ReasonWaitingForProvider, fmt.Sprintf("the config provider has not processed %s %s yet", config.GetKind(), config.GetName())}, nil
	}

	raw, err := json.Marshal(map[string]any{
		"infrastructureTemplateRef": refs.InfrastructureTemplateRef,
		"infrastructureSpec":        infraSpec,
		"configTemplateRef":         refs.ConfigTemplateRef,
		"configReplacementHash":     replacementHash,
		"metadata":                  deployment.Spec.Template.ObjectMeta,
	})
	if err != nil {
		return "", waiting{}, err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])[:hashLength], waiting{}, nil
}

// currentSet returns the RouterSet for hash, creating it with no routers if
// there is none, and brings the settings the deployment passes down to all
// sets up to date.
func (r *Reconciler) currentSet(ctx context.Context, deployment *corev1alpha1.RouterDeployment, sets []*corev1alpha1.RouterSet, hash string) (current *corev1alpha1.RouterSet, all []*corev1alpha1.RouterSet, err error) {
	minReady := ptr.Deref(deployment.Spec.MinReadySeconds, 0)
	highest := 0
	for _, set := range sets {
		highest = max(highest, revision(set))
		if set.Labels[corev1alpha1.TemplateHashLabel] == hash {
			current = set
		}
		if set.Spec.MinReadySeconds != minReady {
			original := set.DeepCopy()
			set.Spec.MinReadySeconds = minReady
			if err := r.Patch(ctx, set, client.MergeFrom(original)); err != nil {
				return nil, nil, err
			}
		}
	}
	if current != nil {
		return current, sets, nil
	}

	templateLabels := map[string]string{}
	for k, v := range deployment.Spec.Template.Labels {
		templateLabels[k] = v
	}
	templateLabels[corev1alpha1.TemplateHashLabel] = hash

	selector := deployment.Spec.Selector.DeepCopy()
	if selector.MatchLabels == nil {
		selector.MatchLabels = map[string]string{}
	}
	// The hash keeps the sets of one deployment from claiming each other's
	// routers during a rollout.
	selector.MatchLabels[corev1alpha1.TemplateHashLabel] = hash

	current = &corev1alpha1.RouterSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deployment.Name + "-" + hash,
			Namespace: deployment.Namespace,
			Labels: map[string]string{
				corev1alpha1.DeploymentNameLabel: deployment.Name,
				corev1alpha1.TemplateHashLabel:   hash,
			},
			Annotations: map[string]string{corev1alpha1.RevisionAnnotation: strconv.Itoa(highest + 1)},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(deployment, corev1alpha1.GroupVersion.WithKind("RouterDeployment")),
			},
		},
		Spec: corev1alpha1.RouterSetSpec{
			Replicas:        ptr.To(int32(0)),
			Selector:        *selector,
			MinReadySeconds: minReady,
			Template: corev1alpha1.RouterTemplateSpec{
				ObjectMeta: corev1alpha1.ObjectMeta{Labels: templateLabels, Annotations: deployment.Spec.Template.Annotations},
				Spec:       deployment.Spec.Template.Spec,
			},
		},
	}
	if err := r.Create(ctx, current); err != nil {
		return nil, nil, fmt.Errorf("create RouterSet: %w", err)
	}
	return current, append(sets, current), nil
}

// roll moves routers from the old sets to the current one within the bounds
// of the strategy: add to the current set as far as the surge allows, and
// take from the old sets only what the group can spare.
func (r *Reconciler) roll(ctx context.Context, deployment *corev1alpha1.RouterDeployment, current *corev1alpha1.RouterSet, sets []*corev1alpha1.RouterSet) error {
	want := ptr.Deref(deployment.Spec.Replicas, 1)
	surge, unavailable := bounds(deployment, want)

	var total, oldTotal int32
	for _, set := range sets {
		total += ptr.Deref(set.Spec.Replicas, 0)
		if set != current {
			oldTotal += ptr.Deref(set.Spec.Replicas, 0)
		}
	}
	currentReplicas := ptr.Deref(current.Spec.Replicas, 0)

	// Up.
	switch {
	case currentReplicas < want:
		if room := want + surge - total; room > 0 {
			scaled := min(want, currentReplicas+room)
			if err := r.scale(ctx, current, scaled); err != nil {
				return err
			}
			total += scaled - currentReplicas
			currentReplicas = scaled
		}
	case currentReplicas > want && oldTotal == 0:
		// The deployment itself was scaled down.
		return r.scale(ctx, current, want)
	}

	// Down. A router of the current set that is not available yet does not
	// carry traffic, so it does not count towards what can be spared.
	currentUnavailable := max(0, currentReplicas-current.Status.AvailableReplicas)
	spare := total - (want - unavailable) - currentUnavailable
	for _, set := range sets {
		if spare <= 0 {
			break
		}
		replicas := ptr.Deref(set.Spec.Replicas, 0)
		if set == current || replicas == 0 {
			continue
		}
		remove := min(spare, replicas)
		if err := r.scale(ctx, set, replicas-remove); err != nil {
			return err
		}
		spare -= remove
	}
	return nil
}

// bounds resolves maxSurge and maxUnavailable to numbers of routers.
func bounds(deployment *corev1alpha1.RouterDeployment, want int32) (surge, unavailable int32) {
	rolling := deployment.Spec.Strategy.RollingUpdate
	// Surge rounds up and unavailable rounds down, both towards safety.
	s, err := intstr.GetScaledValueFromIntOrPercent(ptr.To(ptr.Deref(rolling.MaxSurge, intstr.FromInt32(1))), int(want), true)
	if err != nil || s < 0 {
		s = 1
	}
	u, err := intstr.GetScaledValueFromIntOrPercent(ptr.To(ptr.Deref(rolling.MaxUnavailable, intstr.FromInt32(0))), int(want), false)
	if err != nil || u < 0 {
		u = 0
	}
	if s == 0 && u == 0 {
		// Neither more nor fewer routers allowed: nothing could ever move.
		s = 1
	}
	return int32(s), min(int32(u), want) //nolint:gosec // bounded by the number of replicas
}

func (r *Reconciler) scale(ctx context.Context, set *corev1alpha1.RouterSet, replicas int32) error {
	if ptr.Deref(set.Spec.Replicas, 0) == replicas {
		return nil
	}
	original := set.DeepCopy()
	set.Spec.Replicas = ptr.To(replicas)
	if err := r.Patch(ctx, set, client.MergeFrom(original)); err != nil {
		return fmt.Errorf("scale RouterSet %s to %d: %w", set.Name, replicas, err)
	}
	return nil
}

// trimHistory deletes the oldest emptied sets beyond the history limit.
func (r *Reconciler) trimHistory(ctx context.Context, deployment *corev1alpha1.RouterDeployment, current *corev1alpha1.RouterSet, sets []*corev1alpha1.RouterSet) error {
	var empty []*corev1alpha1.RouterSet
	for _, set := range sets {
		if set != current && ptr.Deref(set.Spec.Replicas, 0) == 0 && set.Status.Replicas == 0 &&
			set.Status.ObservedGeneration >= set.Generation {
			empty = append(empty, set)
		}
	}
	excess := len(empty) - int(ptr.Deref(deployment.Spec.RevisionHistoryLimit, 2))
	for i := 0; i < excess; i++ {
		if err := client.IgnoreNotFound(r.Delete(ctx, empty[i])); err != nil {
			return fmt.Errorf("delete RouterSet %s: %w", empty[i].Name, err)
		}
	}
	return nil
}

// summarize fills in the status and reports whether a rollout is in flight.
// currentName is empty when there is no set for the current template.
func (r *Reconciler) summarize(deployment *corev1alpha1.RouterDeployment, sets []*corev1alpha1.RouterSet, currentName string) bool {
	status := &deployment.Status
	generation := deployment.Generation
	want := ptr.Deref(deployment.Spec.Replicas, 1)
	_, unavailable := bounds(deployment, want)

	status.Replicas, status.UpdatedReplicas, status.ReadyReplicas, status.AvailableReplicas = 0, 0, 0, 0
	rolling := false
	for _, set := range sets {
		status.Replicas += set.Status.Replicas
		status.ReadyReplicas += set.Status.ReadyReplicas
		status.AvailableReplicas += set.Status.AvailableReplicas
		if set.Name == currentName {
			status.UpdatedReplicas = set.Status.Replicas
			// Still rolling while routers of the current set are missing,
			// settling, or run a configuration that is being replaced.
			if ptr.Deref(set.Spec.Replicas, 0) != want || set.Status.AvailableReplicas < want ||
				set.Status.UpToDateReplicas < want || set.Status.ObservedGeneration < set.Generation {
				rolling = true
			}
		} else if ptr.Deref(set.Spec.Replicas, 0) > 0 || set.Status.Replicas > 0 {
			rolling = true
		}
	}

	if status.AvailableReplicas >= want-unavailable {
		conditions.True(&status.Conditions, generation, corev1alpha1.AvailableCondition, ReasonAvailable, "")
	} else {
		conditions.False(&status.Conditions, generation, corev1alpha1.AvailableCondition, ReasonNotEnoughRouters,
			fmt.Sprintf("%d of %d routers are available", status.AvailableReplicas, want))
	}
	if currentName == "" {
		return false
	}
	if rolling {
		conditions.True(&status.Conditions, generation, corev1alpha1.RollingOutCondition, ReasonRollingOut,
			fmt.Sprintf("%d of %d routers are up to date", status.UpdatedReplicas, want))
	} else {
		conditions.False(&status.Conditions, generation, corev1alpha1.RollingOutCondition, ReasonRolledOut, "")
	}
	return rolling
}
