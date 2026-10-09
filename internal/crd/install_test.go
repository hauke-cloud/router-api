package crd

import (
	"slices"
	"testing"

	configv1alpha1 "github.com/hauke-cloud/router-api/api/config/v1alpha1"
	corev1alpha1 "github.com/hauke-cloud/router-api/api/core/v1alpha1"
	infrav1alpha1 "github.com/hauke-cloud/router-api/api/infrastructure/v1alpha1"
)

func TestDefinitionsAreSplitByGroup(t *testing.T) {
	tests := map[string][]string{
		corev1alpha1.GroupVersion.Group: {
			"routerdeployments.router.hauke.cloud", "routerexposures.router.hauke.cloud",
			"routerhealthchecks.router.hauke.cloud",
			"routermachines.router.hauke.cloud",
			"routers.router.hauke.cloud",
			"routersets.router.hauke.cloud",
		},
		infrav1alpha1.GroupVersion.Group: {
			"hetznermachines.infrastructure.router.hauke.cloud",
			"hetznermachinetemplates.infrastructure.router.hauke.cloud",
			"hetznerrouternetworks.infrastructure.router.hauke.cloud",
		},
		configv1alpha1.GroupVersion.Group: {
			"vyosconfigs.config.router.hauke.cloud",
			"vyosconfigtemplates.config.router.hauke.cloud",
		},
	}
	for group, want := range tests {
		t.Run(group, func(t *testing.T) {
			definitions, err := Definitions(group)
			if err != nil {
				t.Fatalf("Definitions: %v", err)
			}
			got := make([]string, 0, len(definitions))
			for _, definition := range definitions {
				if definition.GetKind() != "CustomResourceDefinition" {
					t.Errorf("%s: kind = %q", definition.GetName(), definition.GetKind())
				}
				got = append(got, definition.GetName())
			}
			slices.Sort(got)
			if !slices.Equal(got, want) {
				t.Errorf("definitions = %v, want %v", got, want)
			}
		})
	}
}

func TestDefinitionsOfUnknownGroup(t *testing.T) {
	// A manager that installs nothing would start, find no types and fail
	// much later with a far less obvious error.
	if _, err := Definitions("nope.example.com"); err == nil {
		t.Fatal("Definitions for a group without CRDs succeeded")
	}
}
