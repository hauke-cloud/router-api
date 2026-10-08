// Package managers defines the three managers of router-api: which
// controllers each runs, which API groups it serves. The binaries in cmd/ run
// these definitions, and so does the system test, so that what is tested is
// what is shipped.
package managers

import (
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"

	configv1alpha1 "github.com/hauke-cloud/router-api/api/config/v1alpha1"
	corev1alpha1 "github.com/hauke-cloud/router-api/api/core/v1alpha1"
	infrav1alpha1 "github.com/hauke-cloud/router-api/api/infrastructure/v1alpha1"
	vyoscontroller "github.com/hauke-cloud/router-api/internal/config/vyos/controller"
	"github.com/hauke-cloud/router-api/internal/core/healthcheck"
	"github.com/hauke-cloud/router-api/internal/core/router"
	"github.com/hauke-cloud/router-api/internal/core/routerdeployment"
	"github.com/hauke-cloud/router-api/internal/core/routermachine"
	"github.com/hauke-cloud/router-api/internal/core/routerset"
	"github.com/hauke-cloud/router-api/internal/infrastructure/hetzner/cloud"
	hetznercontroller "github.com/hauke-cloud/router-api/internal/infrastructure/hetzner/controller"
	"github.com/hauke-cloud/router-api/internal/manager"
)

type controller interface {
	SetupWithManager(mgr ctrl.Manager) error
}

func setup(mgr ctrl.Manager, controllers ...controller) error {
	for _, c := range controllers {
		if err := c.SetupWithManager(mgr); err != nil {
			return err
		}
	}
	return nil
}

// Core is the manager of the provider-independent controllers.
func Core() *manager.Definition {
	return &manager.Definition{
		Name:        "router-api-core",
		Groups:      []string{corev1alpha1.GroupVersion.Group},
		AddToScheme: []func(*runtime.Scheme) error{corev1alpha1.AddToScheme},
		Setup: func(mgr ctrl.Manager) error {
			c := mgr.GetClient()
			return setup(mgr,
				&routermachine.Reconciler{Client: c},
				&router.Reconciler{Client: c},
				&routerset.Reconciler{Client: c},
				&routerdeployment.Reconciler{Client: c},
				&healthcheck.Reconciler{Client: c},
			)
		},
	}
}

// Hetzner is the manager of the Hetzner Cloud infrastructure provider.
func Hetzner(factory cloud.Factory) *manager.Definition {
	return &manager.Definition{
		Name: "router-api-infrastructure-hetzner",
		// The core group too: this provider reads RouterMachines.
		Groups:      []string{corev1alpha1.GroupVersion.Group, infrav1alpha1.GroupVersion.Group},
		AddToScheme: []func(*runtime.Scheme) error{corev1alpha1.AddToScheme, infrav1alpha1.AddToScheme},
		Setup: func(mgr ctrl.Manager) error {
			c := mgr.GetClient()
			return setup(mgr,
				&hetznercontroller.NetworkReconciler{Client: c, Cloud: factory},
				&hetznercontroller.MachineReconciler{Client: c, Cloud: factory},
			)
		},
	}
}

// VyOS is the manager of the VyOS config provider.
func VyOS() *manager.Definition {
	return &manager.Definition{
		Name: "router-api-config-vyos",
		// The core group too: this provider reads Routers.
		Groups:      []string{corev1alpha1.GroupVersion.Group, configv1alpha1.GroupVersion.Group},
		AddToScheme: []func(*runtime.Scheme) error{corev1alpha1.AddToScheme, configv1alpha1.AddToScheme},
		Setup: func(mgr ctrl.Manager) error {
			c := mgr.GetClient()
			return setup(mgr,
				&vyoscontroller.Reconciler{Client: c},
				&vyoscontroller.TemplateReconciler{Client: c},
			)
		},
	}
}
