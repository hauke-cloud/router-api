// Command core runs the provider-independent controllers of router-api:
// RouterDeployment, RouterSet, Router, RouterMachine and RouterHealthCheck.
package main

import (
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"

	corev1alpha1 "github.com/hauke-cloud/router-api/api/core/v1alpha1"
	"github.com/hauke-cloud/router-api/internal/core/healthcheck"
	"github.com/hauke-cloud/router-api/internal/core/router"
	"github.com/hauke-cloud/router-api/internal/core/routerdeployment"
	"github.com/hauke-cloud/router-api/internal/core/routermachine"
	"github.com/hauke-cloud/router-api/internal/core/routerset"
	"github.com/hauke-cloud/router-api/internal/manager"
)

func main() {
	manager.Main(&manager.Definition{
		Name:        "router-api-core",
		Groups:      []string{corev1alpha1.GroupVersion.Group},
		AddToScheme: []func(*runtime.Scheme) error{corev1alpha1.AddToScheme},
		Setup: func(mgr ctrl.Manager) error {
			c := mgr.GetClient()
			for _, controller := range []interface{ SetupWithManager(ctrl.Manager) error }{
				&routermachine.Reconciler{Client: c},
				&router.Reconciler{Client: c},
				&routerset.Reconciler{Client: c},
				&routerdeployment.Reconciler{Client: c},
				&healthcheck.Reconciler{Client: c},
			} {
				if err := controller.SetupWithManager(mgr); err != nil {
					return err
				}
			}
			return nil
		},
	})
}
