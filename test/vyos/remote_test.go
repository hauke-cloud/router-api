package vyos

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"

	configv1alpha1 "github.com/hauke-cloud/router-api/api/config/v1alpha1"
	"github.com/hauke-cloud/router-api/internal/config/vyos/apply"
	"github.com/hauke-cloud/router-api/internal/config/vyos/bootstrap"
	"github.com/hauke-cloud/router-api/internal/config/vyos/client"
	"github.com/hauke-cloud/router-api/internal/config/vyos/command"
	"github.com/hauke-cloud/router-api/internal/config/vyos/render"
	"github.com/hauke-cloud/router-api/internal/examples"
)

// TestHomeLabExampleOnAServer commits the whole home-lab example on a router
// that already exists: a real server bootstrapped by router-api's user data,
// as `go run ./hack/userdata` produces it. Unlike a rootless container such a
// router can commit firewall, NAT and WireGuard configuration, so this is
// where those parts of the example are proven.
//
//	ROUTER_ADDRESS=<ip> ROUTER_CREDENTIALS=<directory written by hack/userdata> \
//	  go test -run TestHomeLabExampleOnAServer ./test/vyos/
//
// ROUTER_EXTRA_COMMANDS adds "set" commands, for instance to keep SSH to the
// host open while debugging: the firewall VyOS manages is the host's.
func TestHomeLabExampleOnAServer(t *testing.T) {
	address, directory := os.Getenv("ROUTER_ADDRESS"), os.Getenv("ROUTER_CREDENTIALS")
	if address == "" || directory == "" {
		t.Skip("ROUTER_ADDRESS and ROUTER_CREDENTIALS are not set")
	}
	read := func(name string) []byte {
		raw, err := os.ReadFile(directory + "/" + name) //nolint:gosec // a path given by whoever runs the test
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	credentials := &bootstrap.Credentials{
		APIKey: string(read("api-key")), CertPEM: read("tls.crt"), ServerName: string(read("server-name")),
	}
	api, err := client.New(&client.Options{
		URL: "https://" + address, Key: credentials.APIKey, CACertPEM: credentials.CertPEM, ServerName: credentials.ServerName,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// The management part of the desired configuration has to be what the
	// router was seeded with, key included; only the private key of its
	// certificate is not known here, so that one command is taken from the
	// router.
	running, err := api.Commands(ctx)
	if err != nil {
		t.Fatalf("the router does not accept the credentials: %v", err)
	}
	info, err := api.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("connected to %s, VyOS %s, %d commands", info.Hostname, info.Version, len(running))
	privateKey := command.Path{"pki", "certificate", "router-api", "private", "key"}
	params := &bootstrap.Params{
		Credentials: credentials, HostName: info.Hostname, Port: 443,
		PublicInterface: "eth0", PrivateInterface: "eth1",
	}

	objects, err := examples.Objects("home-lab")
	if err != nil {
		t.Fatal(err)
	}
	template := &configv1alpha1.VyOSConfigTemplate{}
	for _, object := range objects {
		if object.GetKind() == "VyOSConfigTemplate" {
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, template); err != nil {
				t.Fatal(err)
			}
		}
	}
	spec := &template.Spec.Template.Spec
	data := examples.SampleData(spec)
	data.Router.Name = info.Hostname
	if internal := os.Getenv("ROUTER_INTERNAL_ADDRESS"); internal != "" {
		data.Machine.InternalIP = internal
	}
	// ROUTER_PEERS is a comma separated list of the private addresses of the
	// other routers of the group; ROUTER_VALUES overrides template values as
	// name=value pairs.
	if peers := os.Getenv("ROUTER_PEERS"); peers != "" {
		data.Peers = nil
		for i, peer := range strings.Split(peers, ",") {
			data.Peers = append(data.Peers, render.Peer{Name: fmt.Sprintf("peer-%d", i), InternalIP: peer})
		}
	}
	for _, pair := range strings.Split(os.Getenv("ROUTER_VALUES"), ",") {
		if name, value, ok := strings.Cut(pair, "="); ok {
			data.Values[name] = value
		}
	}
	user, err := examples.CommandsWith(spec, data)
	if err != nil {
		t.Fatal(err)
	}
	user = append(user, mustParse(t, os.Getenv("ROUTER_EXTRA_COMMANDS"))...)

	desired := bootstrap.Desired(user, params)
	desired = slices.DeleteFunc(desired, func(path command.Path) bool { return path.HasPrefix(privateKey) })
	for _, path := range running {
		if path.HasPrefix(privateKey) {
			desired = append(desired, path)
		}
	}

	outcome, err := apply.Apply(ctx, api, desired, bootstrap.Keep, 2)
	if err != nil {
		t.Fatalf("the router refuses the example: %v", err)
	}
	t.Logf("applied, changed=%v", outcome.Changed)

	// The operator is still in contact, by construction: the change was
	// confirmed over this connection. And nothing is left to do.
	changes, _, err := apply.Plan(ctx, api, desired, bootstrap.Keep)
	if err != nil {
		t.Fatalf("the router cannot be reached after the change: %v", err)
	}
	for _, change := range changes {
		t.Errorf("not idempotent, a second apply would: %s", redact(change.String()))
	}

	for _, show := range [][]string{{"vrrp"}, {"interfaces"}, {"bgp", "summary"}, {"firewall", "summary"}, {"nat", "source", "rules"}} {
		out, err := api.Show(ctx, show...)
		if err != nil {
			t.Errorf("show %s: %v", strings.Join(show, " "), err)
			continue
		}
		t.Logf("show %s:\n%s", strings.Join(show, " "), out)
	}
}
