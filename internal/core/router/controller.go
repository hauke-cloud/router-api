// Package router reconciles Routers: it joins a machine and a configuration
// into one object that says whether the router works.
package router

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1alpha1 "github.com/hauke-cloud/router-api/api/core/v1alpha1"
	"github.com/hauke-cloud/router-api/internal/conditions"
	"github.com/hauke-cloud/router-api/internal/contract"
)

// Condition reasons set by this controller.
const (
	ReasonMachineNotFound    = "MachineNotFound"
	ReasonConfigNotFound     = "ConfigNotFound"
	ReasonWaitingForProvider = "WaitingForProvider"
	ReasonWaitingForMachine  = "WaitingForMachine"
	ReasonBootstrapPublished = "BootstrapDataPublished"
	ReasonMachineNotReady    = "MachineNotReady"
	ReasonConfigNotApplied   = "ConfigNotApplied"
	ReasonUnhealthy          = "Unhealthy"
	ReasonReady              = "Ready"
	ReasonNotDraining        = "NotDraining"
	ReasonPaused             = "Paused"
	ReasonNotPaused          = "NotPaused"
	ReasonDeleting           = "Deleting"
)

// waitInterval is how soon to look again while waiting for a config object,
// whose kind core does not know and therefore cannot watch.
const waitInterval = 15 * time.Second

// Reconciler reconciles Routers.
type Reconciler struct {
	client.Client
}

// SetupWithManager registers the controller.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.Router{}).
		Owns(&corev1alpha1.RouterMachine{}).
		Complete(r)
}

// Reconcile brings one Router up to date.
func (r *Reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	router := &corev1alpha1.Router{}
	if err := r.Get(ctx, req.NamespacedName, router); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	if !router.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, router)
	}

	if controllerutil.AddFinalizer(router, corev1alpha1.RouterFinalizer) {
		if err := r.Update(ctx, router); err != nil {
			return reconcile.Result{}, err
		}
	}

	original := router.DeepCopy()
	result, err := r.reconcileNormal(ctx, router)
	if statusErr := r.patchStatus(ctx, router, original); statusErr != nil && err == nil {
		err = statusErr
	}
	return result, err
}

func (r *Reconciler) reconcileNormal(ctx context.Context, router *corev1alpha1.Router) (reconcile.Result, error) {
	status := &router.Status
	generation := router.Generation
	status.ObservedGeneration = generation

	if _, paused := router.Annotations[corev1alpha1.PausedAnnotation]; paused {
		conditions.True(&status.Conditions, generation, corev1alpha1.PausedCondition, ReasonPaused, "the paused annotation is set")
		return reconcile.Result{}, nil
	}
	conditions.False(&status.Conditions, generation, corev1alpha1.PausedCondition, ReasonNotPaused, "")

	machine := &corev1alpha1.RouterMachine{}
	err := r.Get(ctx, types.NamespacedName{Namespace: router.Namespace, Name: router.Spec.MachineRef.Name}, machine)
	if apierrors.IsNotFound(err) {
		r.notFound(router, ReasonMachineNotFound, fmt.Sprintf("RouterMachine %s does not exist", router.Spec.MachineRef.Name))
		return reconcile.Result{RequeueAfter: waitInterval}, nil
	} else if err != nil {
		return reconcile.Result{}, err
	}

	config, err := contract.Get(ctx, r.Client, router.Namespace, router.Spec.ConfigRef)
	if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
		r.notFound(router, ReasonConfigNotFound, fmt.Sprintf("%s %s does not exist", router.Spec.ConfigRef.Kind, router.Spec.ConfigRef.Name))
		return reconcile.Result{RequeueAfter: waitInterval}, nil
	} else if err != nil {
		return reconcile.Result{}, err
	}

	if err := r.adoptMachine(ctx, router, machine, config); err != nil {
		return reconcile.Result{}, err
	}
	if err := r.adoptConfig(ctx, router, config); err != nil {
		return reconcile.Result{}, err
	}

	// Bootstrap data.
	if contract.BootstrapDataSecretCreated(config) && contract.BootstrapDataSecretName(config) != "" {
		conditions.True(&status.Conditions, generation, corev1alpha1.BootstrapReadyCondition, ReasonBootstrapPublished, "")
	} else {
		conditions.False(&status.Conditions, generation, corev1alpha1.BootstrapReadyCondition, ReasonWaitingForProvider,
			"the config provider has not published bootstrap data yet")
	}

	// Machine.
	status.Addresses = machine.Status.Addresses
	conditions.Mirror(&status.Conditions, generation, corev1alpha1.MachineReadyCondition,
		conditions.Get(machine.Status.Conditions, corev1alpha1.ReadyCondition),
		ReasonWaitingForMachine, "the machine has not reported yet")

	// Config. A verdict about an older generation of the config object is
	// about a configuration that is no longer the wanted one.
	status.Version = contract.Version(config)
	for _, conditionType := range []string{corev1alpha1.ConfigAppliedCondition, corev1alpha1.HealthyCondition} {
		conditions.Mirror(&status.Conditions, generation, conditionType, current(config, conditionType),
			ReasonWaitingForProvider, "the config provider has not reported yet")
	}
	if _, draining := router.Annotations[corev1alpha1.DrainAnnotation]; draining {
		conditions.Mirror(&status.Conditions, generation, corev1alpha1.DrainedCondition, current(config, corev1alpha1.DrainedCondition),
			ReasonWaitingForProvider, "the config provider has not reported yet")
	} else {
		conditions.False(&status.Conditions, generation, corev1alpha1.DrainedCondition, ReasonNotDraining, "")
	}

	summarize(router)
	return reconcile.Result{RequeueAfter: waitInterval}, nil
}

// current returns a condition of a provider object unless it was reached for
// an older generation of that object.
func current(object *unstructured.Unstructured, conditionType string) *metav1.Condition {
	condition := contract.Condition(object, conditionType)
	if condition != nil && condition.ObservedGeneration != 0 && condition.ObservedGeneration < object.GetGeneration() {
		return nil
	}
	return condition
}

func (r *Reconciler) notFound(router *corev1alpha1.Router, reason, message string) {
	conditions.False(&router.Status.Conditions, router.Generation, corev1alpha1.ReadyCondition, reason, message)
	if router.Status.Phase == "" {
		router.Status.Phase = corev1alpha1.RouterPhasePending
	}
}

// summarize derives Ready and the phase from the other conditions.
func summarize(router *corev1alpha1.Router) {
	status := &router.Status
	generation := router.Generation
	wasReady := status.Phase == corev1alpha1.RouterPhaseReady || status.Phase == corev1alpha1.RouterPhaseDegraded ||
		status.Phase == corev1alpha1.RouterPhaseDraining

	machineReady := conditions.IsTrue(status.Conditions, corev1alpha1.MachineReadyCondition)
	applied := conditions.IsTrue(status.Conditions, corev1alpha1.ConfigAppliedCondition)
	healthy := conditions.IsTrue(status.Conditions, corev1alpha1.HealthyCondition)

	switch {
	case !machineReady:
		conditions.False(&status.Conditions, generation, corev1alpha1.ReadyCondition, ReasonMachineNotReady,
			message(status.Conditions, corev1alpha1.MachineReadyCondition))
	case !applied:
		conditions.False(&status.Conditions, generation, corev1alpha1.ReadyCondition, ReasonConfigNotApplied,
			message(status.Conditions, corev1alpha1.ConfigAppliedCondition))
	case !healthy:
		conditions.False(&status.Conditions, generation, corev1alpha1.ReadyCondition, ReasonUnhealthy,
			message(status.Conditions, corev1alpha1.HealthyCondition))
	default:
		conditions.True(&status.Conditions, generation, corev1alpha1.ReadyCondition, ReasonReady, "")
	}

	_, draining := router.Annotations[corev1alpha1.DrainAnnotation]
	switch {
	case draining:
		status.Phase = corev1alpha1.RouterPhaseDraining
	case machineReady && applied && healthy:
		status.Phase = corev1alpha1.RouterPhaseReady
	case wasReady:
		status.Phase = corev1alpha1.RouterPhaseDegraded
	case machineReady:
		status.Phase = corev1alpha1.RouterPhaseConfiguring
	case conditions.IsTrue(status.Conditions, corev1alpha1.BootstrapReadyCondition):
		status.Phase = corev1alpha1.RouterPhaseProvisioning
	default:
		status.Phase = corev1alpha1.RouterPhasePending
	}
}

// message explains a condition that is not True, falling back to its reason.
func message(all []metav1.Condition, conditionType string) string {
	condition := conditions.Get(all, conditionType)
	if condition == nil {
		return conditionType + " has not been reported"
	}
	if condition.Message != "" {
		return condition.Message
	}
	return fmt.Sprintf("%s is %s: %s", conditionType, condition.Status, condition.Reason)
}

// adoptMachine makes the router the owner of its machine and hands it the
// bootstrap data once the config provider has published it.
func (r *Reconciler) adoptMachine(ctx context.Context, router *corev1alpha1.Router, machine *corev1alpha1.RouterMachine, config *unstructured.Unstructured) error {
	original := machine.DeepCopy()

	if !metav1.IsControlledBy(machine, router) {
		if err := controllerutil.SetControllerReference(router, machine, r.Scheme()); err != nil {
			return fmt.Errorf("own RouterMachine %s: %w", machine.Name, err)
		}
	}
	if name := contract.BootstrapDataSecretName(config); name != "" && contract.BootstrapDataSecretCreated(config) &&
		machine.Spec.Bootstrap.DataSecretName == nil {
		// Set once. The instance was created from this data; pointing the
		// machine at different data later would describe a server that does
		// not exist.
		machine.Spec.Bootstrap.DataSecretName = &name
	}

	if equality.Semantic.DeepEqual(original.ObjectMeta, machine.ObjectMeta) && equality.Semantic.DeepEqual(original.Spec, machine.Spec) {
		return nil
	}
	return r.Patch(ctx, machine, client.MergeFrom(original))
}

// adoptConfig makes the router the owner of its config and keeps the drain
// request on it in step with the router's.
func (r *Reconciler) adoptConfig(ctx context.Context, router *corev1alpha1.Router, config *unstructured.Unstructured) error {
	original := config.DeepCopy()

	if !metav1.IsControlledBy(config, router) {
		if err := controllerutil.SetControllerReference(router, config, r.Scheme()); err != nil {
			return fmt.Errorf("own %s %s: %w", config.GetKind(), config.GetName(), err)
		}
	}
	annotations := config.GetAnnotations()
	if request, draining := router.Annotations[corev1alpha1.DrainAnnotation]; draining {
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[corev1alpha1.DrainAnnotation] = request
	} else {
		delete(annotations, corev1alpha1.DrainAnnotation)
	}
	if len(annotations) == 0 {
		annotations = nil
	}
	config.SetAnnotations(annotations)

	if equality.Semantic.DeepEqual(original.Object["metadata"], config.Object["metadata"]) {
		return nil
	}
	return r.Patch(ctx, config, client.MergeFrom(original))
}

// reconcileDelete removes the machine first and the config after it. The
// config holds the router's credentials; while the server exists they have to
// stay, or a running router would be left with keys nobody holds any more.
func (r *Reconciler) reconcileDelete(ctx context.Context, router *corev1alpha1.Router) (reconcile.Result, error) {
	if !controllerutil.ContainsFinalizer(router, corev1alpha1.RouterFinalizer) {
		return reconcile.Result{}, nil
	}

	machine := &corev1alpha1.RouterMachine{}
	err := r.Get(ctx, types.NamespacedName{Namespace: router.Namespace, Name: router.Spec.MachineRef.Name}, machine)
	switch {
	case err == nil:
		if machine.DeletionTimestamp.IsZero() {
			if err := client.IgnoreNotFound(r.Delete(ctx, machine)); err != nil {
				return reconcile.Result{}, err
			}
		}
		return r.waitForDeletion(ctx, router, "RouterMachine "+machine.Name)
	case !apierrors.IsNotFound(err):
		return reconcile.Result{}, err
	}

	config, err := contract.Get(ctx, r.Client, router.Namespace, router.Spec.ConfigRef)
	switch {
	case err == nil:
		if config.GetDeletionTimestamp().IsZero() {
			if err := client.IgnoreNotFound(r.Delete(ctx, config)); err != nil {
				return reconcile.Result{}, err
			}
		}
		return r.waitForDeletion(ctx, router, config.GetKind()+" "+config.GetName())
	case !apierrors.IsNotFound(err) && !meta.IsNoMatchError(err):
		return reconcile.Result{}, err
	}

	controllerutil.RemoveFinalizer(router, corev1alpha1.RouterFinalizer)
	return reconcile.Result{}, r.Update(ctx, router)
}

func (r *Reconciler) waitForDeletion(ctx context.Context, router *corev1alpha1.Router, what string) (reconcile.Result, error) {
	original := router.DeepCopy()
	router.Status.Phase = corev1alpha1.RouterPhaseDeleting
	conditions.False(&router.Status.Conditions, router.Generation, corev1alpha1.ReadyCondition, ReasonDeleting,
		"waiting for "+what+" to be deleted")
	if err := r.patchStatus(ctx, router, original); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{RequeueAfter: waitInterval / 3}, nil
}

func (r *Reconciler) patchStatus(ctx context.Context, router, original *corev1alpha1.Router) error {
	if equality.Semantic.DeepEqual(router.Status, original.Status) {
		return nil
	}
	return client.IgnoreNotFound(r.Status().Patch(ctx, router, client.MergeFrom(original)))
}
