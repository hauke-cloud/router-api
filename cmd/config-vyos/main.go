// Command config-vyos runs the VyOS config provider of router-api:
// VyOSConfig and VyOSConfigTemplate.
package main

import (
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"

	configv1alpha1 "github.com/hauke-cloud/router-api/api/config/v1alpha1"
	corev1alpha1 "github.com/hauke-cloud/router-api/api/core/v1alpha1"
	"github.com/hauke-cloud/router-api/internal/config/vyos/controller"
	"github.com/hauke-cloud/router-api/internal/manager"
)

func main() {
	manager.Main(&manager.Definition{
		Name: "router-api-config-vyos",
		// The core group too: this provider reads Routers.
		Groups:      []string{corev1alpha1.GroupVersion.Group, configv1alpha1.GroupVersion.Group},
		AddToScheme: []func(*runtime.Scheme) error{corev1alpha1.AddToScheme, configv1alpha1.AddToScheme},
		Setup: func(mgr ctrl.Manager) error {
			if err := (&controller.Reconciler{Client: mgr.GetClient()}).SetupWithManager(mgr); err != nil {
				return err
			}
			return (&controller.TemplateReconciler{Client: mgr.GetClient()}).SetupWithManager(mgr)
		},
	})
}
