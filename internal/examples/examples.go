// Package examples loads the manifests under examples/ for the tests that
// keep them honest.
package examples

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	configv1alpha1 "github.com/hauke-cloud/router-api/api/config/v1alpha1"
	"github.com/hauke-cloud/router-api/internal/config/vyos/command"
	"github.com/hauke-cloud/router-api/internal/config/vyos/render"
)

// Dir returns the path of an example directory.
func Dir(name string) string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "examples", name)
}

// Objects reads every object from the YAML files of an example.
func Objects(name string) ([]*unstructured.Unstructured, error) {
	files, err := filepath.Glob(filepath.Join(Dir(name), "*.yaml"))
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("example %s has no manifests", name)
	}
	var objects []*unstructured.Unstructured
	for _, file := range files {
		raw, err := os.ReadFile(file) //nolint:gosec // paths below examples/
		if err != nil {
			return nil, err
		}
		decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
		for {
			object := &unstructured.Unstructured{}
			if err := decoder.Decode(&object.Object); err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				return nil, fmt.Errorf("%s: %w", filepath.Base(file), err)
			}
			if len(object.Object) > 0 {
				objects = append(objects, object)
			}
		}
	}
	return objects, nil
}

// SampleData is what an example's templates are rendered with in tests: every
// value from a Secret replaced by something of the right shape, and addresses
// a router of a pair would have.
func SampleData(spec *configv1alpha1.VyOSConfigSpec) *render.Data {
	values := map[string]string{}
	// The shared values, and over them those of slot 0.
	all := spec.Values
	if len(spec.Slots) > 0 {
		all = append(append([]configv1alpha1.Value{}, all...), spec.Slots[0].Values...)
	}
	for i := range all {
		value := &all[i]
		switch {
		case value.Value != nil:
			values[value.Name] = *value.Value
		case strings.Contains(strings.ToLower(value.Name), "key"):
			// A syntactically valid WireGuard key.
			values[value.Name] = "kKMnp6QBm1Wj0zxnc1uBbSs3hhdpVIFeQy9hxRTBxWQ="
		default:
			values[value.Name] = "sample-" + value.Name
		}
	}
	// Placeholders in an example are not valid keys either.
	for name, value := range values {
		if strings.HasPrefix(value, "REPLACE_") {
			values[name] = "xo2pQF7WbTFQkAOgNi6s9RNdu1uEhn2wGYOvUkb7HBg="
		}
	}
	return &render.Data{
		Values:  values,
		Router:  render.Router{Name: "edge-abc12", Namespace: "routers", Group: "edge"},
		Machine: render.Machine{ExternalIP: "203.0.113.7", InternalIP: "10.0.1.2"},
		Peers:   []render.Peer{{Name: "edge-def34", Slot: 1, ExternalIP: "203.0.113.8", InternalIP: "10.0.1.3"}},
		Host:    render.Host{PublicInterface: "eth0", PrivateInterface: "eth1"},
		// What two Gateways would add up to: a list and a range of ports,
		// both protocols, both families.
		Exposed: []render.Exposed{
			{Address: "203.0.113.18", Family: "ipv4", Protocol: "tcp", Ports: "80,443,8000-8010"},
			{Address: "203.0.113.18", Family: "ipv4", Protocol: "udp", Ports: "53"},
			{Address: "2001:db8::18", Family: "ipv6", Protocol: "tcp", Ports: "443"},
		},
	}
}

// Commands renders the configuration of an example's VyOSConfigTemplate with
// SampleData.
func Commands(spec *configv1alpha1.VyOSConfigSpec) ([]command.Path, error) {
	return render.Commands(spec.Commands, SampleData(spec))
}

// CommandsWith renders it with the given data.
func CommandsWith(spec *configv1alpha1.VyOSConfigSpec, data *render.Data) ([]command.Path, error) {
	return render.Commands(spec.Commands, data)
}
