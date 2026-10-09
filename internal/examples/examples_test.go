package examples

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configv1alpha1 "github.com/hauke-cloud/router-api/api/config/v1alpha1"
	"github.com/hauke-cloud/router-api/internal/config/vyos/bootstrap"
	"github.com/hauke-cloud/router-api/internal/config/vyos/render"
	"github.com/hauke-cloud/router-api/internal/testenv"
)

var k8s client.Client

func TestMain(m *testing.M) {
	os.Exit(testenv.Run(m, func(c client.Client) { k8s = c }))
}

func template(t *testing.T, name string) *configv1alpha1.VyOSConfigTemplate {
	t.Helper()
	objects, err := Objects(name)
	if err != nil {
		t.Fatal(err)
	}
	for _, object := range objects {
		if object.GetKind() != "VyOSConfigTemplate" {
			continue
		}
		template := &configv1alpha1.VyOSConfigTemplate{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructuredWithValidation(object.Object, template, true); err != nil {
			t.Fatalf("VyOSConfigTemplate %s: %v", object.GetName(), err)
		}
		return template
	}
	t.Fatalf("example %s has no VyOSConfigTemplate", name)
	return nil
}

// The API server is the judge of an example: every object has to pass the
// CRD's schema and validation rules, with no field it does not know.
func TestHomeLabIsAccepted(t *testing.T) {
	objects, err := Objects("home-lab")
	if err != nil {
		t.Fatal(err)
	}
	namespace := testenv.Namespace(t, k8s)
	strict := client.FieldValidation("Strict")
	kinds := map[string]int{}
	for _, object := range objects {
		object.SetNamespace(namespace)
		if err := k8s.Create(context.Background(), object, strict); err != nil {
			t.Errorf("%s %s: %v", object.GetKind(), object.GetName(), err)
		}
		kinds[object.GetKind()]++
	}
	for _, kind := range []string{"HetznerRouterNetwork", "HetznerMachineTemplate", "VyOSConfigTemplate", "RouterDeployment", "RouterHealthCheck", "RouterExposure"} {
		if kinds[kind] == 0 {
			t.Errorf("the example has no %s", kind)
		}
	}
}

func TestHomeLabRenders(t *testing.T) {
	spec := &template(t, "home-lab").Spec.Template.Spec

	paths, err := Commands(spec)
	if err != nil {
		t.Fatalf("the configuration does not render: %v", err)
	}
	if len(paths) < 40 {
		t.Errorf("only %d commands rendered", len(paths))
	}

	// The files, too: the failover helper refuses a configuration it cannot
	// parse, and that would show at the first failover.
	data := SampleData(spec)
	for i := range spec.Files {
		file := &spec.Files[i]
		if file.Value == nil {
			t.Errorf("%s: the example should carry its files inline", file.Path)
			continue
		}
		rendered, err := render.Text(file.Path, *file.Value, data)
		if err != nil {
			t.Errorf("%s: %v", file.Path, err)
			continue
		}
		if strings.HasSuffix(file.Path, ".json") {
			var parsed map[string]any
			if err := json.Unmarshal([]byte(rendered), &parsed); err != nil {
				t.Errorf("%s is not JSON once rendered: %v\n%s", file.Path, err, rendered)
			}
		}
	}

	// And the user data it all ends up in has to fit.
	credentials, err := bootstrap.NewCredentials("edge.routers.router-api.internal")
	if err != nil {
		t.Fatal(err)
	}
	params := &bootstrap.Params{
		Credentials: credentials, Image: spec.Image, HostName: "edge-abc12", Port: 443,
		PublicInterface: "eth0", PrivateInterface: "eth1",
	}
	for i := range spec.Files {
		rendered, _ := render.Text(spec.Files[i].Path, *spec.Files[i].Value, data)
		params.Files = append(params.Files, bootstrap.File{Path: spec.Files[i].Path, Permissions: spec.Files[i].Permissions, Content: rendered})
	}
	userData, err := bootstrap.UserData(params)
	if err != nil {
		t.Fatalf("UserData: %v", err)
	}
	t.Logf("user data is %d of %d bytes", len(userData), bootstrap.MaxUserDataBytes)
}
