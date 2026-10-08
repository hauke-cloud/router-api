package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	configv1alpha1 "github.com/hauke-cloud/router-api/api/config/v1alpha1"
)

// TemplateReconciler publishes on every VyOSConfigTemplate the hash of the
// fields a running router cannot change. Core replaces routers when that
// hash changes, and has the rest applied in place.
type TemplateReconciler struct {
	client.Client
}

// SetupWithManager registers the controller.
func (r *TemplateReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&configv1alpha1.VyOSConfigTemplate{}).
		Complete(r)
}

// ReplacementHash covers what goes into the user data a server is created
// with: the image, the files and the host's interface names. Commands,
// values and the management settings are applied through the API.
func ReplacementHash(spec *configv1alpha1.VyOSConfigSpec) (string, error) {
	raw, err := json.Marshal(struct {
		Image string                  `json:"image"`
		Files []configv1alpha1.File   `json:"files"`
		Host  configv1alpha1.HostSpec `json:"host"`
	}{spec.Image, spec.Files, spec.Host})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])[:16], nil
}

// Reconcile publishes the replacement hash of one template.
func (r *TemplateReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	template := &configv1alpha1.VyOSConfigTemplate{}
	if err := r.Get(ctx, req.NamespacedName, template); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	hash, err := ReplacementHash(&template.Spec.Template.Spec)
	if err != nil {
		return reconcile.Result{}, err
	}
	if template.Status.ReplacementHash == hash && template.Status.ObservedGeneration == template.Generation {
		return reconcile.Result{}, nil
	}
	original := template.DeepCopy()
	template.Status.ReplacementHash = hash
	template.Status.ObservedGeneration = template.Generation
	return reconcile.Result{}, client.IgnoreNotFound(r.Status().Patch(ctx, template, client.MergeFrom(original)))
}
