// Package vyos runs the config provider's building blocks against a real
// VyOS container: the seed script, the REST client and the apply logic.
//
// It needs podman and the VyOS image, so it only runs when VYOS_IMAGE is set
// (`make test-vyos`). Under rootless podman VyOS cannot commit firewall or
// WireGuard configuration, which is why this test stays away from both.
package vyos

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
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
	"github.com/hauke-cloud/router-api/internal/config/vyos/vrrp"
	"github.com/hauke-cloud/router-api/internal/examples"
)

func podman(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.CommandContext(context.Background(), "podman", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("podman %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	return listener.Addr().(*net.TCPAddr).Port
}

func mustParse(t *testing.T, text string) []command.Path {
	t.Helper()
	paths, err := command.Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

// router is a real VyOS container that was seeded by nothing but the
// first-boot hook.
type router struct {
	name   string
	params *bootstrap.Params
	api    *client.Client
}

// boot starts a VyOS container from the seed script and waits until its API
// accepts the generated credentials.
func boot(t *testing.T) *router {
	t.Helper()
	image := os.Getenv("VYOS_IMAGE")
	if image == "" {
		t.Skip("VYOS_IMAGE is not set; run `make test-vyos`")
	}

	credentials, err := bootstrap.NewCredentials("edge.test.router-api.internal")
	if err != nil {
		t.Fatal(err)
	}
	// No interface fallbacks: the container has no eth0 to configure.
	params := &bootstrap.Params{Credentials: credentials, Image: image, HostName: "edge", Port: 443}

	// The bootstrap data's only job is to put this file into the volume.
	volume := t.TempDir()
	if err := os.MkdirAll(filepath.Join(volume, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(volume, "scripts", "vyos-postconfig-bootup.script")
	if err := os.WriteFile(hook, []byte(bootstrap.SeedScript(params)), 0o755); err != nil { //nolint:gosec // has to be executable
		t.Fatal(err)
	}

	name := fmt.Sprintf("router-api-test-%d-%d", os.Getpid(), time.Now().UnixNano()%100000)
	port := freePort(t)
	podman(t, "run", "-d", "--name", name, "--hostname", "edge", "--privileged",
		"-v", "/lib/modules:/lib/modules:ro", "-v", volume+":/opt/vyatta/etc/config",
		"-p", fmt.Sprintf("127.0.0.1:%d:443", port), image)
	t.Cleanup(func() {
		if t.Failed() {
			out, _ := exec.CommandContext(context.Background(), "podman", "exec", name, "journalctl", "-b", "--no-pager", "-n", "40").CombinedOutput()
			t.Logf("journal:\n%s", out)
		}
		_ = exec.CommandContext(context.Background(), "podman", "rm", "-f", name).Run()
		// The container wrote into the volume as users this process is not.
		_ = exec.CommandContext(context.Background(), "podman", "unshare", "rm", "-rf", volume).Run()
	})

	api, err := client.New(&client.Options{
		URL: fmt.Sprintf("https://127.0.0.1:%d", port), Key: credentials.APIKey,
		CACertPEM: credentials.CertPEM, ServerName: credentials.ServerName,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Nothing but the seed script has touched the router. If the API
	// answers with our certificate and accepts our key, the seed worked.
	ctx := context.Background()
	var info client.Info
	deadline := time.Now().Add(3 * time.Minute)
	for {
		if info, err = api.Info(ctx); err == nil {
			if _, err = api.Commands(ctx); err == nil {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the seeded router did not come up: %v", err)
		}
		time.Sleep(3 * time.Second)
	}
	t.Logf("VyOS %s is up", info.Version)
	return &router{name: name, params: params, api: api}
}

func TestSeedAndApplyOnRealVyOS(t *testing.T) {
	r := boot(t)
	router, params := r.api, r.params
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	user := `
set system time-zone Europe/Berlin
set interfaces dummy dum0 address 192.0.2.1/24
set protocols bgp system-as 65001
set protocols bgp neighbor 192.0.2.2 remote-as 65001
set protocols bgp neighbor 192.0.2.2 address-family ipv4-unicast
set service monitoring prometheus node-exporter listen-address 127.0.0.1
set high-availability vrrp group wan vrid 10
set high-availability vrrp group wan interface dum0
set high-availability vrrp group wan address 192.0.2.100/24
set high-availability vrrp group wan hello-source-address 192.0.2.1
set high-availability vrrp group wan peer-address 192.0.2.9
`
	desired := bootstrap.Desired(mustParse(t, user), params)

	outcome, err := apply.Apply(ctx, router, desired, bootstrap.Keep, 1)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !outcome.Changed {
		t.Error("Apply reports no change")
	}

	// What the router runs now has to contain everything asked for.
	running, err := router.Commands(ctx)
	if err != nil {
		t.Fatal(err)
	}
	lines := command.Lines(running)
	for _, want := range command.Lines(desired) {
		if !slices.Contains(lines, want) {
			t.Errorf("the router does not run %q", redact(want))
		}
	}

	// The real test of the diff: a second apply of the same configuration
	// must find nothing to do. If VyOS stored anything differently from how
	// it was asked, this is where it shows.
	changes, _, err := apply.Plan(ctx, router, desired, bootstrap.Keep)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range changes {
		t.Errorf("not idempotent, a second apply would: %s", redact(change.String()))
	}

	// The VRRP group is alone on its interface and becomes master.
	var state string
	for range 20 {
		out, err := router.Show(ctx, "vrrp")
		if err != nil {
			t.Fatal(err)
		}
		if state, _, _ = vrrp.Summary(vrrp.Parse(out)); state == vrrp.StateMaster {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if state != vrrp.StateMaster {
		t.Errorf("VRRP state = %s, want MASTER", state)
	}

	// Removing something removes it, and leaves the management part alone.
	shorter := bootstrap.Desired(mustParse(t, "set system time-zone Europe/Berlin\n"), params)
	if _, err := apply.Apply(ctx, router, shorter, bootstrap.Keep, 1); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	running, err = router.Commands(ctx)
	if err != nil {
		t.Fatalf("the router is unreachable after removing configuration: %v", err)
	}
	for _, line := range command.Lines(running) {
		if strings.Contains(line, "bgp") || strings.Contains(line, "dum0") || strings.Contains(line, "vrrp") {
			t.Errorf("still configured: %s", line)
		}
	}

	// A change that is rejected leaves the router as it was.
	broken := append(slices.Clone(shorter), command.Path{"interfaces", "ethernet", "eth9", "address", "192.0.2.9/24"})
	before := command.Hash(running)
	_, err = apply.Apply(ctx, router, broken, bootstrap.Keep, 1)
	var commitErr *apply.CommitError
	if !errors.As(err, &commitErr) {
		t.Fatalf("configuring an interface that does not exist: err = %v, want a CommitError", err)
	}
	running, err = router.Commands(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if command.Hash(running) != before {
		t.Error("the rejected change left something behind")
	}
}

// redact hides values of lines that carry secrets.
func redact(line string) string {
	for _, marker := range []string{" key ", " certificate router-api certificate ", "password"} {
		if i := strings.Index(line, marker); i >= 0 {
			return line[:i+len(marker)] + "<redacted>"
		}
	}
	return line
}

// rootlessCannotCommit are the parts of a configuration that need the host's
// kernel in ways a rootless container is not allowed: netfilter and sysctls,
// and the WireGuard module. With a rootful engine there is no such list.
var rootlessCannotCommit = []command.Path{
	{"firewall"},
	{"nat"},
	{"interfaces", "wireguard"},
}

func rootless(t *testing.T) bool {
	t.Helper()
	return podman(t, "info", "--format", "{{.Host.Security.Rootless}}") == "true"
}

// TestHomeLabExampleOnRealVyOS checks the configuration in examples/home-lab
// against the VyOS it is meant for. An example that the router rejects is
// worse than no example.
func TestHomeLabExampleOnRealVyOS(t *testing.T) {
	r := boot(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

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
	paths, err := examples.Commands(&template.Spec.Template.Spec)
	if err != nil {
		t.Fatalf("the example does not render: %v", err)
	}

	// Syntax first, for all of it. The check has to be able to fail, so it
	// is first shown a command VyOS must reject.
	if rejected := validate(t, r, []command.Path{{"system", "time-zone", "UTC"}, {"firewall", "ipv4", "input", "filter", "rule", "10", "state", "no-such-state"}}); len(rejected) != 1 || rejected[0] != 1 {
		t.Fatalf("the validation does not catch an invalid command: rejected = %v", rejected)
	}
	for _, index := range validate(t, r, paths) {
		t.Errorf("VyOS rejects: %s", redact(paths[index].String()))
	}
	if t.Failed() {
		t.FailNow()
	}

	// Then commit what this engine can commit.
	desired := bootstrap.Desired(paths, r.params)
	if rootless(t) {
		desired = slices.DeleteFunc(desired, func(path command.Path) bool {
			for _, prefix := range rootlessCannotCommit {
				if path.HasPrefix(prefix) {
					return true
				}
			}
			// What refers to the tunnel goes with the tunnel.
			return slices.Contains(path, "wg0") || slices.Contains(path, "10.99.0.1")
		})
		t.Log("rootless engine: firewall, NAT and WireGuard are validated but not committed")
	}
	// The container has neither of the host's interfaces.
	desired = slices.DeleteFunc(desired, func(path command.Path) bool { return slices.Contains(path, "eth0") || slices.Contains(path, "eth1") })
	desired = append(desired, mustParse(t, "set interfaces dummy dum1 address 10.0.1.2/24\nset high-availability vrrp group wan interface dum1\n")...)

	if _, err := apply.Apply(ctx, r.api, desired, bootstrap.Keep, 1); err != nil {
		t.Fatalf("VyOS refuses to commit the example: %v", err)
	}
	changes, _, err := apply.Plan(ctx, r.api, desired, bootstrap.Keep)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range changes {
		t.Errorf("not idempotent, a second apply would: %s", redact(change.String()))
	}
}

// validate has VyOS check commands without applying them: each is "set" in a
// configuration session that is then discarded. VyOS validates a command when
// it is set, so this needs nothing from the kernel. It returns the indexes of
// the commands VyOS rejected.
//
// "set" exits 0 in a script even when it rejects its arguments, so the verdict
// has to be read from what it prints: a marker goes out before every command,
// and a "Set failed" belongs to the marker before it.
func validate(t *testing.T, r *router, paths []command.Path) []int {
	t.Helper()
	var script strings.Builder
	script.WriteString("#!/bin/vbash\nsource /opt/vyatta/etc/functions/script-template\nconfigure\n")
	for i, path := range paths {
		fmt.Fprintf(&script, "echo \"@@ %d\"\n%s\n", i, path.String())
	}
	script.WriteString("echo \"@@ done\"\nexit discard\n")
	scriptPath := filepath.Join(t.TempDir(), "validate.sh")
	if err := os.WriteFile(scriptPath, []byte(script.String()), 0o755); err != nil { //nolint:gosec // has to be executable
		t.Fatal(err)
	}
	podman(t, "cp", scriptPath, r.name+":/tmp/validate.sh")
	out := podman(t, "exec", r.name, "sg", "vyattacfg", "-c", "/tmp/validate.sh")
	if !strings.Contains(out, "@@ done") {
		t.Fatalf("the validation script did not run to its end:\n%s", out)
	}

	var rejected []int
	current := -1
	for _, line := range strings.Split(out, "\n") {
		var index int
		if _, err := fmt.Sscanf(strings.TrimSpace(line), "@@ %d", &index); err == nil {
			current = index
			continue
		}
		if strings.Contains(line, "Set failed") && current >= 0 && !slices.Contains(rejected, current) {
			rejected = append(rejected, current)
		}
	}
	return rejected
}
