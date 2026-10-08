// Command infrastructure-hetzner runs the Hetzner Cloud infrastructure
// provider of router-api: HetznerRouterNetwork and HetznerMachine.
package main

import (
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"

	corev1alpha1 "github.com/hauke-cloud/router-api/api/core/v1alpha1"
	infrav1alpha1 "github.com/hauke-cloud/router-api/api/infrastructure/v1alpha1"
	"github.com/hauke-cloud/router-api/internal/infrastructure/hetzner/cloud"
	"github.com/hauke-cloud/router-api/internal/infrastructure/hetzner/controller"
	"github.com/hauke-cloud/router-api/internal/manager"
	"github.com/hauke-cloud/router-api/internal/version"
)

func main() {
	manager.Main(&manager.Definition{
		Name: "router-api-infrastructure-hetzner",
		// The core group too: this provider reads RouterMachines.
		Groups:      []string{corev1alpha1.GroupVersion.Group, infrav1alpha1.GroupVersion.Group},
		AddToScheme: []func(*runtime.Scheme) error{corev1alpha1.AddToScheme, infrav1alpha1.AddToScheme},
		Setup: func(mgr ctrl.Manager) error {
			factory := cloud.NewFactory(cloud.WithApplication("router-api", version.Version()))
			if err := (&controller.NetworkReconciler{Client: mgr.GetClient(), Cloud: factory}).SetupWithManager(mgr); err != nil {
				return err
			}
			return (&controller.MachineReconciler{Client: mgr.GetClient(), Cloud: factory}).SetupWithManager(mgr)
		},
	})
}
