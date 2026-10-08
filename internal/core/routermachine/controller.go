// Package routermachine reconciles RouterMachines: it ties a machine to the
// provider object that creates its instance and reports that object's state
// in provider-independent terms.
package routermachine

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
	ReasonInfrastructureNotFound = "InfrastructureNotFound"
	ReasonWaitingForProvider     = "WaitingForProvider"
	ReasonPaused                 = "Paused"
	ReasonNotPaused              = "NotPaused"
	ReasonDeleting               = "Deleting"
)

// waitInterval is how soon to look again while waiting for something that
// raises no event this controller watches.
const waitInterval = 15 * time.Second

// Reconciler reconciles RouterMachines.
type Reconciler struct {
	client.Client
}

// SetupWithManager registers the controller. Infrastructure objects are of
// kinds core does not know at compile time, so they are not watched; the
// machine is polled while it waits and the provider's status changes reach it
// within waitInterval.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.RouterMachine{}).
		Complete(r)
}

// Reconcile brings one RouterMachine up to date.
func (r *Reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	machine := &corev1alpha1.RouterMachine{}
	if err := r.Get(ctx, req.NamespacedName, machine); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	if !machine.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, machine)
	}

	original := machine.DeepCopy()
	result, err := r.reconcileNormal(ctx, machine)
	if statusErr := r.patchStatus(ctx, machine, original); statusErr != nil && err == nil {
		err = statusErr
	}
	return result, err
}

func (r *Reconciler) reconcileNormal(ctx context.Context, machine *corev1alpha1.RouterMachine) (reconcile.Result, error) {
	status := &machine.Status
	status.ObservedGeneration = machine.Generation

	if _, paused := machine.Annotations[corev1alpha1.PausedAnnotation]; paused {
		conditions.True(&status.Conditions, machine.Generation, corev1alpha1.PausedCondition, ReasonPaused, "the paused annotation is set")
		return reconcile.Result{}, nil
	}
	conditions.False(&status.Conditions, machine.Generation, corev1alpha1.PausedCondition, ReasonNotPaused, "")

	if controllerutil.AddFinalizer(machine, corev1alpha1.RouterMachineFinalizer) {
		if err := r.Update(ctx, machine); err != nil {
			return reconcile.Result{}, err
		}
		status = &machine.Status
		status.ObservedGeneration = machine.Generation
	}

	infra, err := contract.Get(ctx, r.Client, machine.Namespace, machine.Spec.InfrastructureRef)
	if err != nil {
		if !apierrors.IsNotFound(err) && !meta.IsNoMatchError(err) {
			return reconcile.Result{}, err
		}
		message := fmt.Sprintf("%s %s does not exist", machine.Spec.InfrastructureRef.Kind, machine.Spec.InfrastructureRef.Name)
		conditions.False(&status.Conditions, machine.Generation, corev1alpha1.InfrastructureReadyCondition, ReasonInfrastructureNotFound, message)
		conditions.False(&status.Conditions, machine.Generation, corev1alpha1.ReadyCondition, ReasonInfrastructureNotFound, message)
		status.Phase = phase(machine)
		return reconcile.Result{RequeueAfter: waitInterval}, nil
	}

	if err := r.adopt(ctx, machine, infra); err != nil {
		return reconcile.Result{}, err
	}

	// Mirror what the provider reports.
	if providerID := contract.ProviderID(infra); providerID != "" && machine.Spec.ProviderID != providerID {
		machine.Spec.ProviderID = providerID
		saved := machine.Status
		if err := r.Update(ctx, machine); err != nil {
			return reconcile.Result{}, err
		}
		machine.Status = saved
		status = &machine.Status
		status.ObservedGeneration = machine.Generation
	}
	// Provisioned only ever goes from false to true: it records that an
	// instance was created, not that it is currently fine.
	status.InfrastructureProvisioned = status.InfrastructureProvisioned || contract.InfrastructureProvisioned(infra)
	status.Addresses = contract.Addresses(infra)
	status.FailureDomain = contract.FailureDomain(infra)
	status.LastRemediation = contract.LastRemediation(infra)

	infraReady := contract.Condition(infra, corev1alpha1.ReadyCondition)
	if infraReady != nil && infraReady.ObservedGeneration != 0 && infraReady.ObservedGeneration < infra.GetGeneration() {
		// A verdict about an older spec of the infrastructure object.
		infraReady = nil
	}
	conditions.Mirror(&status.Conditions, machine.Generation, corev1alpha1.InfrastructureReadyCondition, infraReady,
		ReasonWaitingForProvider, "the infrastructure provider has not reported yet")
	conditions.Mirror(&status.Conditions, machine.Generation, corev1alpha1.ReadyCondition, infraReady,
		ReasonWaitingForProvider, "the infrastructure provider has not reported yet")
	status.Phase = phase(machine)

	return reconcile.Result{RequeueAfter: waitInterval}, nil
}

// adopt makes the machine the controlling owner of its infrastructure object
// and passes on a remediation request.
func (r *Reconciler) adopt(ctx context.Context, machine *corev1alpha1.RouterMachine, infra *unstructured.Unstructured) error {
	original := infra.DeepCopy()

	if !metav1.IsControlledBy(infra, machine) {
		if err := controllerutil.SetControllerReference(machine, infra, r.Scheme()); err != nil {
			return fmt.Errorf("own %s %s: %w", infra.GetKind(), infra.GetName(), err)
		}
	}
	if request, ok := machine.Annotations[corev1alpha1.RemediationAnnotation]; ok {
		annotations := infra.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[corev1alpha1.RemediationAnnotation] = request
		infra.SetAnnotations(annotations)
	}

	if equality.Semantic.DeepEqual(original.Object["metadata"], infra.Object["metadata"]) {
		return nil
	}
	return r.Patch(ctx, infra, client.MergeFrom(original))
}

func (r *Reconciler) reconcileDelete(ctx context.Context, machine *corev1alpha1.RouterMachine) (reconcile.Result, error) {
	if !controllerutil.ContainsFinalizer(machine, corev1alpha1.RouterMachineFinalizer) {
		return reconcile.Result{}, nil
	}

	infra, err := contract.Get(ctx, r.Client, machine.Namespace, machine.Spec.InfrastructureRef)
	switch {
	case apierrors.IsNotFound(err) || meta.IsNoMatchError(err):
		// Gone, or of a kind that no longer exists: nothing left to wait for.
		controllerutil.RemoveFinalizer(machine, corev1alpha1.RouterMachineFinalizer)
		return reconcile.Result{}, r.Update(ctx, machine)
	case err != nil:
		return reconcile.Result{}, err
	}

	if infra.GetDeletionTimestamp().IsZero() {
		if err := client.IgnoreNotFound(r.Delete(ctx, infra)); err != nil {
			return reconcile.Result{}, err
		}
	}

	original := machine.DeepCopy()
	machine.Status.Phase = corev1alpha1.RouterMachinePhaseDeleting
	conditions.False(&machine.Status.Conditions, machine.Generation, corev1alpha1.ReadyCondition, ReasonDeleting,
		fmt.Sprintf("waiting for %s %s to be deleted", infra.GetKind(), infra.GetName()))
	if err := r.patchStatus(ctx, machine, original); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{RequeueAfter: waitInterval / 3}, nil
}

func (r *Reconciler) patchStatus(ctx context.Context, machine, original *corev1alpha1.RouterMachine) error {
	if equality.Semantic.DeepEqual(machine.Status, original.Status) {
		return nil
	}
	// The spec may have been updated since original was copied; only the
	// status is to be compared and sent.
	base := machine.DeepCopy()
	base.Status = original.Status
	return client.IgnoreNotFound(r.Status().Patch(ctx, machine, client.MergeFrom(base)))
}

func phase(machine *corev1alpha1.RouterMachine) corev1alpha1.RouterMachinePhase {
	switch {
	case conditions.IsTrue(machine.Status.Conditions, corev1alpha1.ReadyCondition):
		return corev1alpha1.RouterMachinePhaseRunning
	case machine.Status.InfrastructureProvisioned:
		// Created, and not fine right now.
		return corev1alpha1.RouterMachinePhaseFailed
	case machine.Spec.Bootstrap.DataSecretName != nil:
		return corev1alpha1.RouterMachinePhaseProvisioning
	default:
		return corev1alpha1.RouterMachinePhasePending
	}
}
