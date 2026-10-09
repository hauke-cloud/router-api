// Package system runs the three managers of router-api together, the way
// they run in a cluster, against a real API server.
//
// Hetzner Cloud is the in-memory fake. Each server it "creates" is a fake
// VyOS router that is started from the user data the config provider
// generated: the test extracts the API key and the certificate from the seed
// script in it, exactly as a real first boot would apply them. If the
// operator can then reach and configure that router, the bootstrap data, the
// credentials and the provider contract all fit together.
package system

import (
	"context"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	"sigs.k8s.io/yaml"

	configv1alpha1 "github.com/hauke-cloud/router-api/api/config/v1alpha1"
	corev1alpha1 "github.com/hauke-cloud/router-api/api/core/v1alpha1"
	infrav1alpha1 "github.com/hauke-cloud/router-api/api/infrastructure/v1alpha1"
	"github.com/hauke-cloud/router-api/internal/conditions"
	"github.com/hauke-cloud/router-api/internal/config/vyos/command"
	"github.com/hauke-cloud/router-api/internal/config/vyos/vyostest"
	"github.com/hauke-cloud/router-api/internal/infrastructure/hetzner/cloud"
	"github.com/hauke-cloud/router-api/internal/infrastructure/hetzner/cloud/fake"
	"github.com/hauke-cloud/router-api/internal/manager"
	"github.com/hauke-cloud/router-api/internal/managers"
	"github.com/hauke-cloud/router-api/internal/pace"
	"github.com/hauke-cloud/router-api/internal/testenv"
)

var (
	k8s        client.Client
	restConfig *rest.Config
)

func TestMain(m *testing.M) {
	os.Exit(testenv.RunWithConfig(m, func(cfg *rest.Config, c client.Client) { restConfig, k8s = cfg, c }))
}

// datacenter turns servers of the fake cloud into fake routers.
type datacenter struct {
	t    *testing.T
	port int

	mu      sync.Mutex
	next    byte
	routers map[string]*vyostest.Router
}

func newDatacenter(t *testing.T) *datacenter {
	t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	return &datacenter{t: t, port: port, next: 10, routers: map[string]*vyostest.Router{}}
}

// boot is the first boot of a server: the seed script from the user data is
// what configures the router.
func (d *datacenter) boot(spec *cloud.ServerSpec, server *cloud.Server) {
	d.mu.Lock()
	defer d.mu.Unlock()

	seed := seedScript(d.t, spec.UserData)
	var (
		config  strings.Builder
		options vyostest.Options
	)
	for _, line := range strings.Split(seed, "\n") {
		if !strings.HasPrefix(line, "set ") {
			continue
		}
		path, err := command.ParseLine(line)
		if err != nil {
			d.t.Errorf("seed script line %q: %v", line, err)
			continue
		}
		config.WriteString(line + "\n")
		switch {
		case path.HasPrefix(command.Path{"service", "https", "api", "keys", "id", "router-api", "key"}):
			options.Key = path[len(path)-1]
		case path.HasPrefix(command.Path{"pki", "certificate", "router-api", "certificate"}):
			options.CertPEM = armour(d.t, "CERTIFICATE", path[len(path)-1])
		case path.HasPrefix(command.Path{"pki", "certificate", "router-api", "private", "key"}):
			options.KeyPEM = armour(d.t, "PRIVATE KEY", path[len(path)-1])
		}
	}
	if options.Key == "" || options.CertPEM == nil || options.KeyPEM == nil {
		d.t.Errorf("the seed script of %s does not set an API key and a certificate:\n%s", spec.Name, seed)
		return
	}

	// Every server gets a loopback address of its own, so that the routers
	// can all listen on the one management port. A server that was given a
	// Primary IP already has its address, and listens there: its successor
	// in the slot will listen on the very same one.
	d.next++
	address := netip.AddrFrom4([4]byte{127, 0, 0, d.next})
	if spec.PrimaryIPv4ID != 0 {
		address = server.PublicIPv4
	}
	options.Config = config.String()
	options.ListenAddress = netip.AddrPortFrom(address, uint16(d.port)).String() //nolint:gosec // a port number
	d.routers[spec.Name] = vyostest.Start(d.t, &options)

	server.Status = cloud.ServerStatusRunning
	server.PublicIPv4 = address
	server.PublicIPv6 = netip.Addr{}
}

func (d *datacenter) remove(name string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if router, ok := d.routers[name]; ok {
		router.Stop()
		delete(d.routers, name)
	}
}

func (d *datacenter) router(name string) *vyostest.Router {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.routers[name]
}

// seedScript digs the first-boot hook out of cloud-init user data.
func seedScript(t *testing.T, userData string) string {
	t.Helper()
	var config struct {
		WriteFiles []struct {
			Path, Encoding, Content string
		} `json:"write_files"`
	}
	if err := yaml.Unmarshal([]byte(userData), &config); err != nil {
		t.Errorf("user data is not YAML: %v", err)
		return ""
	}
	for _, file := range config.WriteFiles {
		if !strings.HasSuffix(file.Path, "/scripts/vyos-postconfig-bootup.script") {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(file.Content)
		if err != nil {
			t.Errorf("seed script is not base64: %v", err)
		}
		return string(raw)
	}
	t.Error("the user data has no seed script")
	return ""
}

func armour(t *testing.T, blockType, body string) []byte {
	t.Helper()
	der, err := base64.StdEncoding.DecodeString(body)
	if err != nil {
		t.Errorf("%s in the seed script is not base64: %v", blockType, err)
		return nil
	}
	return pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
}

type system struct {
	t         *testing.T
	ctx       context.Context
	namespace string
	cloud     *fake.Cloud
	dc        *datacenter
}

// start runs the three managers until the test ends.
func start(t *testing.T) *system {
	t.Helper()
	// Polling that takes tens of seconds in production takes a moment here.
	pace.Accelerate(60)
	t.Cleanup(func() { pace.Accelerate(0) })

	ctx, cancel := context.WithCancel(context.Background())
	s := &system{t: t, ctx: ctx, namespace: testenv.Namespace(t, k8s), cloud: fake.New("lab"), dc: newDatacenter(t)}
	s.cloud.OnCreate = s.dc.boot
	s.cloud.OnDelete = s.dc.remove

	logger := slog.New(slog.DiscardHandler)
	if os.Getenv("SYSTEM_TEST_LOG") != "" {
		logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}
	cfg := &manager.Config{MetricsAddress: "0", ProbeAddress: "0", Namespace: s.namespace, InstallCRDs: true, AllowDuplicateControllers: true}

	var wg sync.WaitGroup
	for _, definition := range []*manager.Definition{managers.Core(), managers.Hetzner(s.cloud.Factory()), managers.VyOS()} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := manager.Start(ctx, definition, cfg, restConfig, logger); err != nil {
				t.Errorf("%s: %v", definition.Name, err)
			}
		}()
	}
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
	return s
}

const configCommands = `set system time-zone {{ .Values.zone }}
set high-availability vrrp group wan vrid 10
set high-availability vrrp group wan interface {{ .Host.PrivateInterface }}
set high-availability vrrp group wan hello-source-address {{ default "0.0.0.0" .Machine.InternalIP }}
{{- range .Peers }}
set high-availability vrrp group wan peer-address {{ .InternalIP }}
{{- end }}
`

// customize changes the objects of a group before they are created.
type customize struct {
	network    func(*infrav1alpha1.HetznerRouterNetwork)
	deployment func(*corev1alpha1.RouterDeployment)
	machine    func(*infrav1alpha1.HetznerMachineTemplate)
	config     func(*configv1alpha1.VyOSConfigTemplate)
}

func (s *system) create() {
	s.t.Helper()
	s.createWith(customize{})
}

func (s *system) createWith(c customize) {
	s.t.Helper()
	labels := map[string]string{"app": "edge"}
	fast := metav1.Duration{Duration: 3 * time.Second}
	objects := []client.Object{
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "hcloud", Namespace: s.namespace},
			Data:       map[string][]byte{"token": []byte("s3cret")},
		},
		&infrav1alpha1.HetznerRouterNetwork{
			ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: s.namespace},
			Spec: infrav1alpha1.HetznerRouterNetworkSpec{
				TokenSecretRef: infrav1alpha1.SecretKeyReference{Name: "hcloud"},
				Network:        infrav1alpha1.HetznerNetworkAttachment{Name: "lab"},
				Firewall: infrav1alpha1.HetznerFirewall{
					ManagementPort:    int32(s.dc.port), //nolint:gosec // a port number
					ManagementSources: []infrav1alpha1.ManagementSource{{CIDR: "127.0.0.0/8"}},
				},
			},
		},
		&infrav1alpha1.HetznerMachineTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: s.namespace},
			Spec: infrav1alpha1.HetznerMachineTemplateSpec{Template: infrav1alpha1.HetznerMachineTemplateResource{
				Spec: infrav1alpha1.HetznerMachineSpec{
					NetworkRef: corev1.LocalObjectReference{Name: "edge"}, ServerType: "cx23", Location: "fsn1",
				},
			}},
		},
		&configv1alpha1.VyOSConfigTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: s.namespace},
			Spec: configv1alpha1.VyOSConfigTemplateSpec{Template: configv1alpha1.VyOSConfigTemplateResource{
				Spec: configv1alpha1.VyOSConfigSpec{
					Image:      "ghcr.io/hauke-cloud/vyos@sha256:abc",
					Commands:   configCommands,
					Values:     []configv1alpha1.Value{{Name: "zone", ValueSource: configv1alpha1.ValueSource{Value: ptr.To("UTC")}}},
					Management: configv1alpha1.ManagementSpec{Port: int32(s.dc.port)}, //nolint:gosec // a port number
				},
			}},
		},
		&corev1alpha1.RouterDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: s.namespace},
			Spec: corev1alpha1.RouterDeploymentSpec{
				Replicas:        ptr.To(int32(2)),
				MinReadySeconds: ptr.To(int32(1)),
				Selector:        metav1.LabelSelector{MatchLabels: labels},
				Template: corev1alpha1.RouterTemplateSpec{
					ObjectMeta: corev1alpha1.ObjectMeta{Labels: labels},
					Spec: corev1alpha1.RouterTemplate{
						InfrastructureTemplateRef: corev1alpha1.ContractReference{
							APIGroup: infrav1alpha1.GroupVersion.Group, Kind: "HetznerMachineTemplate", Name: "edge",
						},
						ConfigTemplateRef: corev1alpha1.ContractReference{
							APIGroup: configv1alpha1.GroupVersion.Group, Kind: "VyOSConfigTemplate", Name: "edge",
						},
					},
				},
			},
		},
		&corev1alpha1.RouterHealthCheck{
			ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: s.namespace},
			Spec: corev1alpha1.RouterHealthCheckSpec{
				Selector: metav1.LabelSelector{MatchLabels: labels},
				UnhealthyConditions: []corev1alpha1.UnhealthyCondition{
					{Type: corev1alpha1.ReadyCondition, Status: metav1.ConditionFalse, Timeout: fast},
				},
				Remediation: corev1alpha1.Remediation{RebootAttempts: ptr.To(int32(1)), RebootTimeout: &fast},
			},
		},
	}
	for _, object := range objects {
		switch typed := object.(type) {
		case *infrav1alpha1.HetznerRouterNetwork:
			if c.network != nil {
				c.network(typed)
			}
		case *corev1alpha1.RouterDeployment:
			if c.deployment != nil {
				c.deployment(typed)
			}
		case *infrav1alpha1.HetznerMachineTemplate:
			if c.machine != nil {
				c.machine(typed)
			}
		case *configv1alpha1.VyOSConfigTemplate:
			if c.config != nil {
				c.config(typed)
			}
		}
		if err := k8s.Create(s.ctx, object); err != nil {
			s.t.Fatalf("create %T: %v", object, err)
		}
	}
}

// ready returns the names of the routers that are Ready and not being
// deleted, sorted.
func (s *system) ready() []string {
	s.t.Helper()
	list := &corev1alpha1.RouterList{}
	if err := k8s.List(s.ctx, list, client.InNamespace(s.namespace)); err != nil {
		s.t.Fatal(err)
	}
	var names []string
	for i := range list.Items {
		router := &list.Items[i]
		if router.DeletionTimestamp.IsZero() && conditions.IsTrue(router.Status.Conditions, corev1alpha1.ReadyCondition) {
			names = append(names, router.Name)
		}
	}
	slices.Sort(names)
	return names
}

// existing returns the number of routers that are not being deleted.
func (s *system) existing() int {
	s.t.Helper()
	list := &corev1alpha1.RouterList{}
	if err := k8s.List(s.ctx, list, client.InNamespace(s.namespace)); err != nil {
		s.t.Fatal(err)
	}
	n := 0
	for i := range list.Items {
		if list.Items[i].DeletionTimestamp.IsZero() {
			n++
		}
	}
	return n
}

// eventually polls until done reports true, and fails the test with
// describe's answer if that takes longer than the timeout. always is checked
// on every poll and must never fail.
func (s *system) eventually(what string, timeout time.Duration, done func() bool, always func() string) {
	s.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if always != nil {
			if violation := always(); violation != "" {
				s.t.Fatalf("while waiting for %s: %s", what, violation)
			}
		}
		if done() {
			return
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("timed out after %v waiting for %s\n%s", timeout, what, s.dump())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// dump describes the state of the system, for a failed wait.
func (s *system) dump() string {
	var out strings.Builder
	routers := &corev1alpha1.RouterList{}
	_ = k8s.List(s.ctx, routers, client.InNamespace(s.namespace))
	for i := range routers.Items {
		router := &routers.Items[i]
		fmt.Fprintf(&out, "Router %s phase=%s deleting=%v\n", router.Name, router.Status.Phase, !router.DeletionTimestamp.IsZero())
		for _, condition := range router.Status.Conditions {
			fmt.Fprintf(&out, "  %s=%s %s %s\n", condition.Type, condition.Status, condition.Reason, condition.Message)
		}
	}
	sets := &corev1alpha1.RouterSetList{}
	_ = k8s.List(s.ctx, sets, client.InNamespace(s.namespace))
	for i := range sets.Items {
		set := &sets.Items[i]
		fmt.Fprintf(&out, "RouterSet %s spec=%d status=%+v\n", set.Name, ptr.Deref(set.Spec.Replicas, -1), set.Status)
	}
	configs := &configv1alpha1.VyOSConfigList{}
	_ = k8s.List(s.ctx, configs, client.InNamespace(s.namespace))
	for i := range configs.Items {
		config := &configs.Items[i]
		fmt.Fprintf(&out, "VyOSConfig %s\n", config.Name)
		for _, condition := range config.Status.Conditions {
			fmt.Fprintf(&out, "  %s=%s %s %s\n", condition.Type, condition.Status, condition.Reason, condition.Message)
		}
	}
	machines := &infrav1alpha1.HetznerMachineList{}
	_ = k8s.List(s.ctx, machines, client.InNamespace(s.namespace))
	for i := range machines.Items {
		machine := &machines.Items[i]
		fmt.Fprintf(&out, "HetznerMachine %s server=%s providerID=%s\n", machine.Name, machine.Status.ServerStatus, machine.Spec.ProviderID)
		for _, condition := range machine.Status.Conditions {
			fmt.Fprintf(&out, "  %s=%s %s %s\n", condition.Type, condition.Status, condition.Reason, condition.Message)
		}
	}
	fmt.Fprintf(&out, "servers: %v\n", serverNames(s.cloud))
	return out.String()
}

func serverNames(c *fake.Cloud) []string {
	servers := c.Servers()
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func (s *system) allRun(routers []string, line string) bool {
	for _, name := range routers {
		router := s.dc.router(name)
		if router == nil || !slices.Contains(router.Running(), line) {
			return false
		}
	}
	return true
}

func TestRouterGroupLifecycle(t *testing.T) {
	s := start(t)
	s.create()

	// --- creation -----------------------------------------------------------
	s.eventually("two routers to become ready", 90*time.Second, func() bool { return len(s.ready()) == 2 }, nil)
	first := s.ready()
	if got := serverNames(s.cloud); !slices.Equal(got, first) {
		t.Fatalf("servers = %v, routers = %v", got, first)
	}
	for _, name := range first {
		spec, _ := s.cloud.Spec(name)
		if spec.ServerType != "cx23" || spec.FirewallID == 0 || spec.NetworkID == 0 || spec.PlacementGroupID == 0 {
			t.Errorf("server %s was created with %+v", name, spec)
		}
	}
	// Each router has been told about the other, once both had an address.
	s.eventually("the routers to learn about each other", 60*time.Second, func() bool {
		for _, name := range first {
			router := s.dc.router(name)
			peers := 0
			for _, line := range router.Running() {
				if strings.HasPrefix(line, "set high-availability vrrp group wan peer-address 10.") {
					peers++
				}
			}
			if peers != 1 || !slices.Contains(router.Running(), "set system time-zone UTC") {
				return false
			}
		}
		return true
	}, nil)
	s.eventually("the deployment to be available", 30*time.Second, func() bool {
		deployment := &corev1alpha1.RouterDeployment{}
		if err := k8s.Get(s.ctx, types.NamespacedName{Namespace: s.namespace, Name: "edge"}, deployment); err != nil {
			return false
		}
		return conditions.IsTrue(deployment.Status.Conditions, corev1alpha1.AvailableCondition) &&
			!conditions.IsTrue(deployment.Status.Conditions, corev1alpha1.RollingOutCondition) &&
			deployment.Status.AvailableReplicas == 2
	}, nil)

	// neverShort is the promise of a router group: whatever is being done
	// to it, two routers stay ready.
	neverShort := func() string {
		if ready := s.ready(); len(ready) < 2 {
			return fmt.Sprintf("only %v are ready; the group must never drop below 2", ready)
		}
		return ""
	}

	// --- a configuration change is applied in place -----------------------
	template := &configv1alpha1.VyOSConfigTemplate{}
	if err := k8s.Get(s.ctx, types.NamespacedName{Namespace: s.namespace, Name: "edge"}, template); err != nil {
		t.Fatal(err)
	}
	template.Spec.Template.Spec.Values[0].Value = ptr.To("Europe/Berlin")
	if err := k8s.Update(s.ctx, template); err != nil {
		t.Fatal(err)
	}
	s.eventually("both routers to run the changed configuration", 90*time.Second,
		func() bool { return s.allRun(first, "set system time-zone Europe/Berlin") }, neverShort)
	if got := serverNames(s.cloud); !slices.Equal(got, first) {
		t.Fatalf("servers = %v, want the same %v: a configuration change must not replace servers", got, first)
	}

	// --- a change of the server type replaces the routers one by one -------
	infra := &infrav1alpha1.HetznerMachineTemplate{}
	if err := k8s.Get(s.ctx, types.NamespacedName{Namespace: s.namespace, Name: "edge"}, infra); err != nil {
		t.Fatal(err)
	}
	infra.Spec.Template.Spec.ServerType = "cx33"
	if err := k8s.Update(s.ctx, infra); err != nil {
		t.Fatal(err)
	}
	most := 0
	s.eventually("both routers to be replaced", 3*time.Minute, func() bool {
		most = max(most, s.existing())
		servers := s.cloud.Servers()
		if len(servers) != 2 {
			return false
		}
		for name := range servers {
			if spec, _ := s.cloud.Spec(name); spec.ServerType != "cx33" {
				return false
			}
		}
		return len(s.ready()) == 2
	}, neverShort)
	second := s.ready()
	for _, name := range second {
		if slices.Contains(first, name) {
			t.Errorf("router %s survived the replacement", name)
		}
	}
	// As with Pods, a router that is being deleted no longer counts: its
	// server may still exist for a moment next to its replacement's.
	if most > 3 {
		t.Errorf("up to %d routers existed at once, not counting those being deleted; maxSurge is 1", most)
	}
	if !s.allRun(second, "set system time-zone Europe/Berlin") {
		t.Error("the new routers do not run the current configuration")
	}

	// --- a router that dies is rebooted, then replaced ----------------------
	victim := second[0]
	s.dc.router(victim).Stop()
	s.eventually("the dead router to be rebooted", 60*time.Second, func() bool { return s.cloud.Resets(victim) > 0 }, nil)
	s.eventually("the dead router to be replaced", 2*time.Minute, func() bool {
		ready := s.ready()
		return len(ready) == 2 && !slices.Contains(ready, victim)
	}, func() string {
		// The healthy router must not be touched while its peer is down.
		if !slices.Contains(serverNames(s.cloud), second[1]) {
			return "the healthy router " + second[1] + " was removed"
		}
		return ""
	})
	if _, ok := s.cloud.Servers()[victim]; ok {
		t.Errorf("the server of the dead router %s still exists", victim)
	}

	// --- deletion leaves nothing behind -------------------------------------
	if err := k8s.Delete(s.ctx, &corev1alpha1.RouterDeployment{ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: s.namespace}}); err != nil {
		t.Fatal(err)
	}
	// In a cluster the garbage collector now deletes the RouterSets and the
	// Routers, which the deployment owns. envtest runs no garbage collector,
	// so the test stands in for it. Everything below a Router is deleted by
	// its own finalizer, in order, and that is what is being checked.
	if err := k8s.DeleteAllOf(s.ctx, &corev1alpha1.RouterSet{}, client.InNamespace(s.namespace)); err != nil {
		t.Fatal(err)
	}
	if err := k8s.DeleteAllOf(s.ctx, &corev1alpha1.Router{}, client.InNamespace(s.namespace)); err != nil {
		t.Fatal(err)
	}
	s.eventually("all servers to be deleted", 2*time.Minute, func() bool { return len(s.cloud.Servers()) == 0 }, nil)
	s.eventually("the routers and their configs to be gone", time.Minute, func() bool {
		routers := &corev1alpha1.RouterList{}
		configs := &configv1alpha1.VyOSConfigList{}
		machines := &infrav1alpha1.HetznerMachineList{}
		for _, list := range []client.ObjectList{routers, configs, machines} {
			if err := k8s.List(s.ctx, list, client.InNamespace(s.namespace)); err != nil {
				return false
			}
		}
		return len(routers.Items) == 0 && len(configs.Items) == 0 && len(machines.Items) == 0
	}, nil)

	if err := k8s.Delete(s.ctx, &infrav1alpha1.HetznerRouterNetwork{ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: s.namespace}}); err != nil {
		t.Fatal(err)
	}
	s.eventually("the firewall and placement group to be deleted", time.Minute, func() bool {
		err := k8s.Get(s.ctx, types.NamespacedName{Namespace: s.namespace, Name: "edge"}, &infrav1alpha1.HetznerRouterNetwork{})
		return apierrors.IsNotFound(err) && len(s.cloud.PlacementGroups()) == 0 &&
			s.cloud.Firewall("router-api-"+s.namespace+"-edge") == nil
	}, nil)
}

// bySlot returns the routers that are not being deleted, by slot.
func (s *system) bySlot() map[string]*corev1alpha1.Router {
	s.t.Helper()
	list := &corev1alpha1.RouterList{}
	if err := k8s.List(s.ctx, list, client.InNamespace(s.namespace)); err != nil {
		s.t.Fatal(err)
	}
	out := map[string]*corev1alpha1.Router{}
	for i := range list.Items {
		if list.Items[i].DeletionTimestamp.IsZero() {
			out[list.Items[i].Labels[corev1alpha1.SlotLabel]] = &list.Items[i]
		}
	}
	return out
}

// objects returns the number of Router objects, those being deleted included.
func (s *system) objects() int {
	s.t.Helper()
	list := &corev1alpha1.RouterList{}
	if err := k8s.List(s.ctx, list, client.InNamespace(s.namespace)); err != nil {
		s.t.Fatal(err)
	}
	return len(list.Items)
}

// TestSlots runs a group whose routers each have something of their own that
// has to survive their replacement: a public address and a tunnel. That is
// what lets the other side keep a tunnel to every router, so that a failover
// does not have to wait for one to be set up.
func TestSlots(t *testing.T) {
	s := start(t)
	// The addresses exist before the routers and belong to nobody yet.
	addresses := map[string]string{"0": "127.0.1.10", "1": "127.0.1.11"}
	tunnels := map[string]string{"0": "10.99.0.1/30", "1": "10.99.0.5/30"}
	s.cloud.AddPrimaryIP("edge-0", addresses["0"], false)
	s.cloud.AddPrimaryIP("edge-1", addresses["1"], false)

	s.createWith(customize{
		deployment: func(d *corev1alpha1.RouterDeployment) { d.Spec.Strategy.Type = corev1alpha1.SlotsStrategy },
		machine: func(m *infrav1alpha1.HetznerMachineTemplate) {
			m.Spec.Template.Spec.PrimaryIPv4BySlot = []string{"edge-0", "edge-1"}
		},
		config: func(c *configv1alpha1.VyOSConfigTemplate) {
			c.Spec.Template.Spec.Commands = configCommands + "set interfaces dummy dum1 address {{ .Values.tunnel }}\n"
			for _, slot := range []string{"0", "1"} {
				c.Spec.Template.Spec.Slots = append(c.Spec.Template.Spec.Slots, configv1alpha1.SlotSpec{Values: []configv1alpha1.Value{
					{Name: "tunnel", ValueSource: configv1alpha1.ValueSource{Value: ptr.To(tunnels[slot])}},
				}})
			}
		},
	})

	// correct reports whether both slots hold a ready router with the
	// slot's address that runs the slot's configuration.
	correct := func() bool {
		routers := s.bySlot()
		ready := s.ready()
		for slot, address := range addresses {
			router := routers[slot]
			if router == nil || !slices.Contains(ready, router.Name) {
				return false
			}
			server, ok := s.cloud.Servers()[router.Name]
			if !ok || server.PublicIPv4.String() != address {
				return false
			}
			fake := s.dc.router(router.Name)
			if fake == nil || !slices.Contains(fake.Running(), "set interfaces dummy dum1 address "+tunnels[slot]) {
				return false
			}
		}
		return len(routers) == 2
	}
	s.eventually("a router in each slot, with its address and its tunnel", 90*time.Second, correct, nil)
	first := s.bySlot()

	// --- replacement in place ---------------------------------------------
	// The promise is a different one from the Surge strategy's: never two
	// routers in a slot, and never both slots empty.
	inPlace := func() string {
		if n := s.objects(); n > 2 {
			return fmt.Sprintf("%d routers exist; a slot holds one", n)
		}
		if len(s.ready()) == 0 {
			return "no router is ready; they have to be replaced one after the other"
		}
		return ""
	}
	infra := &infrav1alpha1.HetznerMachineTemplate{}
	if err := k8s.Get(s.ctx, types.NamespacedName{Namespace: s.namespace, Name: "edge"}, infra); err != nil {
		t.Fatal(err)
	}
	infra.Spec.Template.Spec.ServerType = "cx33"
	if err := k8s.Update(s.ctx, infra); err != nil {
		t.Fatal(err)
	}
	s.eventually("both routers to be replaced in their slots", 3*time.Minute, func() bool {
		routers := s.bySlot()
		for slot := range addresses {
			if routers[slot] == nil || routers[slot].Name == first[slot].Name {
				return false
			}
			if spec, _ := s.cloud.Spec(routers[slot].Name); spec.ServerType != "cx33" {
				return false
			}
		}
		return correct()
	}, inPlace)

	// --- a router that dies comes back in its slot -----------------------
	second := s.bySlot()
	s.dc.router(second["1"].Name).Stop()
	s.eventually("the dead router to be replaced in slot 1", 3*time.Minute, func() bool {
		routers := s.bySlot()
		return routers["1"] != nil && routers["1"].Name != second["1"].Name && correct()
	}, func() string {
		if routers := s.bySlot(); routers["0"] == nil || routers["0"].Name != second["0"].Name {
			return "the healthy router in slot 0 was touched"
		}
		return inPlace()
	})
}

// A Gateway asks to be exposed, and both the firewall in front of the routers
// and the routers themselves follow: core collects, the two providers act.
func TestGatewaysDecideWhatIsOpen(t *testing.T) {
	s := start(t)
	ref := &corev1alpha1.LocalObjectReference{Name: "edge"}
	if err := k8s.Create(s.ctx, &corev1alpha1.RouterExposure{ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: s.namespace}}); err != nil {
		t.Fatal(err)
	}
	s.createWith(customize{
		network: func(n *infrav1alpha1.HetznerRouterNetwork) { n.Spec.Firewall.ExposureRef = ref },
		config: func(c *configv1alpha1.VyOSConfigTemplate) {
			c.Spec.Template.Spec.ExposureRef = ref
			c.Spec.Template.Spec.Commands = configCommands + `{{- range $i, $e := .Exposed }}
set firewall {{ $e.Family }} forward filter rule {{ add 100 $i }} action accept
set firewall {{ $e.Family }} forward filter rule {{ add 100 $i }} destination address {{ $e.Address }}
set firewall {{ $e.Family }} forward filter rule {{ add 100 $i }} destination port {{ $e.Ports }}
set firewall {{ $e.Family }} forward filter rule {{ add 100 $i }} protocol {{ $e.Protocol }}
{{- end }}
`
		},
	})
	s.eventually("two routers to become ready", 90*time.Second, func() bool { return len(s.ready()) == 2 }, nil)
	routers := s.ready()

	// open reports whether the Hetzner firewall lets the port through.
	open := func(port string) bool {
		for _, rule := range s.cloud.FirewallRules("router-api-" + s.namespace + "-edge") {
			if rule.Protocol == "tcp" && rule.Port == port && len(rule.Sources) == 2 {
				return true
			}
		}
		return false
	}
	if open("443") || open("8443") {
		t.Fatal("ports are open before any Gateway asked for them")
	}

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name: "web", Namespace: s.namespace,
			Annotations: map[string]string{corev1alpha1.ExposeAnnotation: "edge"},
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "test",
			Listeners:        []gatewayv1.Listener{{Name: "https", Protocol: gatewayv1.HTTPSProtocolType, Port: 443}},
		},
	}
	if err := k8s.Create(s.ctx, gateway); err != nil {
		t.Fatal(err)
	}
	// The Gateway's implementation gives it an address.
	gateway.Status.Addresses = []gatewayv1.GatewayStatusAddress{{Value: "203.0.113.18"}}
	if err := k8s.Status().Update(s.ctx, gateway); err != nil {
		t.Fatal(err)
	}
	s.eventually("the Gateway's port to be open", 60*time.Second, func() bool {
		return open("443") &&
			s.allRun(routers, "set firewall ipv4 forward filter rule 100 destination address 203.0.113.18") &&
			s.allRun(routers, "set firewall ipv4 forward filter rule 100 destination port 443")
	}, nil)

	// A listener more, and nothing but the Gateway was touched.
	if err := k8s.Get(s.ctx, client.ObjectKeyFromObject(gateway), gateway); err != nil {
		t.Fatal(err)
	}
	gateway.Spec.Listeners = append(gateway.Spec.Listeners, gatewayv1.Listener{Name: "alt", Protocol: gatewayv1.HTTPSProtocolType, Port: 8443})
	if err := k8s.Update(s.ctx, gateway); err != nil {
		t.Fatal(err)
	}
	s.eventually("the new listener's port to be open", 60*time.Second, func() bool {
		return open("443") && open("8443") &&
			s.allRun(routers, "set firewall ipv4 forward filter rule 100 destination port 443,8443")
	}, nil)

	// Someone else adds a listener with a ListenerSet, which counts once the
	// Gateway's implementation has accepted it.
	set := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "database", Namespace: s.namespace},
		Spec: gatewayv1.ListenerSetSpec{
			ParentRef: gatewayv1.ParentGatewayReference{Name: "web"},
			Listeners: []gatewayv1.ListenerEntry{{Name: "postgres", Protocol: gatewayv1.TCPProtocolType, Port: 5432}},
		},
	}
	if err := k8s.Create(s.ctx, set); err != nil {
		t.Fatal(err)
	}
	meta.SetStatusCondition(&set.Status.Conditions, metav1.Condition{
		Type: string(gatewayv1.ListenerSetConditionAccepted), Status: metav1.ConditionTrue, Reason: string(gatewayv1.ListenerSetReasonAccepted),
	})
	if err := k8s.Status().Update(s.ctx, set); err != nil {
		t.Fatal(err)
	}
	s.eventually("the ListenerSet's port to be open", 60*time.Second, func() bool {
		return open("5432") &&
			s.allRun(routers, "set firewall ipv4 forward filter rule 100 destination port 443,5432,8443")
	}, nil)

	// The Gateway goes, and so does what was open for it.
	if err := k8s.Delete(s.ctx, gateway); err != nil {
		t.Fatal(err)
	}
	s.eventually("the ports to be closed again", 60*time.Second, func() bool {
		if open("443") || open("8443") || open("5432") {
			return false
		}
		for _, name := range routers {
			for _, line := range s.dc.router(name).Running() {
				if strings.HasPrefix(line, "set firewall ") {
					return false
				}
			}
		}
		return true
	}, func() string {
		if ready := s.ready(); len(ready) < 2 {
			return fmt.Sprintf("only %v are ready: following a Gateway must not take a router out of service", ready)
		}
		return ""
	})
}
