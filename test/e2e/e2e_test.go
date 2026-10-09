// Package e2e runs router-api against Hetzner Cloud for real: the three
// managers, as shipped, create servers, the servers boot VyOS from the
// generated user data, and the operator configures them over the internet.
//
// It creates billed resources and deletes them again, so it only runs when
// asked to (`make e2e`), with:
//
//	HCLOUD_TOKEN          read/write token of a project to test in
//	E2E_NETWORK           name of an existing Hetzner Cloud Network with a subnet
//	E2E_NETWORK_CIDR      its range, e.g. 10.0.0.0/16
//	E2E_MANAGEMENT_CIDR   where this machine's connections come from, e.g. 198.51.100.7/32
//	E2E_VYOS_IMAGE        VyOS container image the servers can pull or already have
//	E2E_SERVER_IMAGE      optional: server image or snapshot ID, default ubuntu-24.04
//	E2E_FLOATING_IP       optional: an unassigned Floating IP that follows the VRRP master
//	E2E_PRIMARY_IPS       optional: names of two unassigned Primary IPs with auto-delete off,
//	                      comma separated. Runs the group with the Slots strategy: each router
//	                      in a slot with its own address, replaced in place.
//	E2E_SSH_KEY           optional: name of a Hetzner SSH key for the hosts, for debugging
//	E2E_KEEP              optional: if the test fails, leave the servers for inspection
//
// Only the Kubernetes API server is not real: the managers run against
// envtest, which is enough for them and needs no cluster.
package e2e

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configv1alpha1 "github.com/hauke-cloud/router-api/api/config/v1alpha1"
	corev1alpha1 "github.com/hauke-cloud/router-api/api/core/v1alpha1"
	infrav1alpha1 "github.com/hauke-cloud/router-api/api/infrastructure/v1alpha1"
	"github.com/hauke-cloud/router-api/internal/conditions"
	"github.com/hauke-cloud/router-api/internal/infrastructure/hetzner/cloud"
	hetznercontroller "github.com/hauke-cloud/router-api/internal/infrastructure/hetzner/controller"
	"github.com/hauke-cloud/router-api/internal/manager"
	"github.com/hauke-cloud/router-api/internal/managers"
	"github.com/hauke-cloud/router-api/internal/testenv"
)

var (
	k8s        client.Client
	restConfig *rest.Config
)

func TestMain(m *testing.M) {
	if os.Getenv("HCLOUD_TOKEN") == "" || os.Getenv("E2E_NETWORK") == "" {
		// Not asked for. The test reports itself as skipped.
		os.Exit(m.Run())
	}
	os.Exit(testenv.RunWithConfig(m, func(cfg *rest.Config, c client.Client) { restConfig, k8s = cfg, c }))
}

func env(t *testing.T, name string) string {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		t.Fatalf("%s is not set", name)
	}
	return value
}

type suite struct {
	t          *testing.T
	ctx        context.Context
	namespace  string
	hetzner    *hcloud.Client
	floatingIP string
	// primaryIPs are the names of the Primary IPs by slot; empty for a
	// group without slots.
	primaryIPs []string
}

func (s *suite) slots() bool { return len(s.primaryIPs) > 0 }

// misplaced returns a description of the first router that is not at the
// Primary IP of its slot, or does not run the values of its slot, or "".
func (s *suite) misplaced() string {
	s.t.Helper()
	servers := s.servers()
	routers := s.routers()
	seen := map[string]bool{}
	for i := range routers {
		router := &routers[i]
		if !router.DeletionTimestamp.IsZero() {
			continue
		}
		label := router.Labels[corev1alpha1.SlotLabel]
		slot, err := strconv.Atoi(label)
		if err != nil || slot >= len(s.primaryIPs) || seen[label] {
			return fmt.Sprintf("router %s has slot %q", router.Name, label)
		}
		seen[label] = true
		primary, _, err := s.hetzner.PrimaryIP.GetByName(s.ctx, s.primaryIPs[slot])
		if err != nil || primary == nil {
			s.t.Fatalf("primary IP %s: %v", s.primaryIPs[slot], err)
		}
		server := servers[router.Name]
		if server == nil {
			return fmt.Sprintf("router %s in slot %d has no server", router.Name, slot)
		}
		if !server.PublicNet.IPv4.IP.Equal(primary.IP) {
			return fmt.Sprintf("router %s in slot %d is at %s, the slot's address is %s", router.Name, slot, server.PublicNet.IPv4.IP, primary.IP)
		}
	}
	if len(seen) != len(s.primaryIPs) {
		return fmt.Sprintf("%d of %d slots are filled", len(seen), len(s.primaryIPs))
	}
	return ""
}

func (s *suite) logf(format string, args ...any) {
	s.t.Helper()
	s.t.Logf("%s "+format, append([]any{time.Now().Format("15:04:05")}, args...)...)
}

// servers returns the servers this provider created, by name.
func (s *suite) servers() map[string]*hcloud.Server {
	s.t.Helper()
	all, err := s.hetzner.Server.AllWithOpts(s.ctx, hcloud.ServerListOpts{
		ListOpts: hcloud.ListOpts{LabelSelector: hetznercontroller.ManagedByLabel + "=" + hetznercontroller.ManagedByValue},
	})
	if err != nil {
		s.t.Fatalf("list servers: %v", err)
	}
	out := map[string]*hcloud.Server{}
	for _, server := range all {
		out[server.Name] = server
	}
	return out
}

func (s *suite) serverNames() []string {
	servers := s.servers()
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// purge deletes whatever the provider created at Hetzner, whether the test
// got as far as cleaning up properly or not.
func (s *suite) purge() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	selector := hcloud.ListOpts{LabelSelector: hetznercontroller.ManagedByLabel + "=" + hetznercontroller.ManagedByValue}
	servers, _ := s.hetzner.Server.AllWithOpts(ctx, hcloud.ServerListOpts{ListOpts: selector})
	for _, server := range servers {
		s.t.Logf("purging server %s", server.Name)
		_, _, _ = s.hetzner.Server.DeleteWithResult(ctx, server)
	}
	for range 30 {
		remaining, _ := s.hetzner.Server.AllWithOpts(ctx, hcloud.ServerListOpts{ListOpts: selector})
		if len(remaining) == 0 {
			break
		}
		time.Sleep(2 * time.Second)
	}
	firewalls, _ := s.hetzner.Firewall.AllWithOpts(ctx, hcloud.FirewallListOpts{ListOpts: selector})
	for _, firewall := range firewalls {
		for range 15 {
			if _, err := s.hetzner.Firewall.Delete(ctx, firewall); err == nil {
				break
			}
			time.Sleep(2 * time.Second)
		}
	}
	groups, _ := s.hetzner.PlacementGroup.AllWithOpts(ctx, hcloud.PlacementGroupListOpts{ListOpts: selector})
	for _, group := range groups {
		_, _ = s.hetzner.PlacementGroup.Delete(ctx, group)
	}
}

func (s *suite) routers() []corev1alpha1.Router {
	s.t.Helper()
	list := &corev1alpha1.RouterList{}
	if err := k8s.List(s.ctx, list, client.InNamespace(s.namespace)); err != nil {
		s.t.Fatal(err)
	}
	return list.Items
}

// ready returns the names of the routers that are Ready and not being
// deleted, sorted.
func (s *suite) ready() []string {
	var names []string
	routers := s.routers()
	for i := range routers {
		router := &routers[i]
		if router.DeletionTimestamp.IsZero() && conditions.IsTrue(router.Status.Conditions, corev1alpha1.ReadyCondition) {
			names = append(names, router.Name)
		}
	}
	slices.Sort(names)
	return names
}

func (s *suite) describe() string {
	var out strings.Builder
	routers := s.routers()
	for i := range routers {
		router := &routers[i]
		fmt.Fprintf(&out, "Router %s phase=%s deleting=%v addresses=%v\n", router.Name, router.Status.Phase, !router.DeletionTimestamp.IsZero(), router.Status.Addresses)
		for _, condition := range router.Status.Conditions {
			fmt.Fprintf(&out, "  %s=%s %s %s\n", condition.Type, condition.Status, condition.Reason, condition.Message)
		}
	}
	configs := &configv1alpha1.VyOSConfigList{}
	_ = k8s.List(s.ctx, configs, client.InNamespace(s.namespace))
	for i := range configs.Items {
		fmt.Fprintf(&out, "VyOSConfig %s\n", configs.Items[i].Name)
		for _, condition := range configs.Items[i].Status.Conditions {
			fmt.Fprintf(&out, "  %s=%s %s %s\n", condition.Type, condition.Status, condition.Reason, condition.Message)
		}
	}
	machines := &infrav1alpha1.HetznerMachineList{}
	_ = k8s.List(s.ctx, machines, client.InNamespace(s.namespace))
	for i := range machines.Items {
		machine := &machines.Items[i]
		fmt.Fprintf(&out, "HetznerMachine %s server=%s\n", machine.Name, machine.Status.ServerStatus)
		for _, condition := range machine.Status.Conditions {
			fmt.Fprintf(&out, "  %s=%s %s %s\n", condition.Type, condition.Status, condition.Reason, condition.Message)
		}
	}
	networks := &infrav1alpha1.HetznerRouterNetworkList{}
	_ = k8s.List(s.ctx, networks, client.InNamespace(s.namespace))
	for i := range networks.Items {
		for _, condition := range networks.Items[i].Status.Conditions {
			fmt.Fprintf(&out, "HetznerRouterNetwork %s=%s %s %s\n", condition.Type, condition.Status, condition.Reason, condition.Message)
		}
	}
	return out.String()
}

func (s *suite) eventually(what string, timeout time.Duration, done func() bool, always func() string) {
	s.t.Helper()
	s.logf("waiting for %s", what)
	deadline := time.Now().Add(timeout)
	for {
		if always != nil {
			if violation := always(); violation != "" {
				s.t.Fatalf("while waiting for %s: %s\n%s", what, violation, s.describe())
			}
		}
		if done() {
			s.logf("done: %s", what)
			return
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("timed out after %v waiting for %s\n%s", timeout, what, s.describe())
		}
		time.Sleep(2 * time.Second)
	}
}

// probe measures how long the Floating IP does not answer. The routers'
// REST API listens on it, so a TCP connect is the cheapest sign of life.
type probe struct {
	mu      sync.Mutex
	longest time.Duration
	down    time.Time
	stop    chan struct{}
	done    chan struct{}
}

func startProbe(address string) *probe {
	p := &probe{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(p.done)
		dialer := &net.Dialer{Timeout: time.Second}
		for {
			select {
			case <-p.stop:
				return
			default:
			}
			conn, err := dialer.DialContext(context.Background(), "tcp", net.JoinHostPort(address, "443"))
			p.mu.Lock()
			if err != nil {
				if p.down.IsZero() {
					p.down = time.Now()
				}
			} else {
				_ = conn.Close()
				if !p.down.IsZero() {
					p.longest = max(p.longest, time.Since(p.down))
					p.down = time.Time{}
				}
			}
			p.mu.Unlock()
			time.Sleep(250 * time.Millisecond)
		}
	}()
	return p
}

func (p *probe) finish() time.Duration {
	close(p.stop)
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.down.IsZero() {
		p.longest = max(p.longest, time.Since(p.down))
	}
	return p.longest
}

const commands = `set system time-zone {{ .Values.zone }}
# The failover helper has to resolve api.hetzner.cloud. These are Hetzner's
# own resolvers.
set system name-server 185.12.64.1
set system name-server 185.12.64.2

# A server's private address is a /32; the rest of the network is behind the
# gateway, and VyOS's DHCP client does not install the route.
set protocols static route {{ .Values.gateway }}/32 interface {{ .Host.PrivateInterface }}
set protocols static route {{ .Values.network }} next-hop {{ .Values.gateway }}

set high-availability vrrp group wan vrid 10
set high-availability vrrp group wan interface {{ .Host.PrivateInterface }}
set high-availability vrrp group wan address 10.255.255.1/32
set high-availability vrrp group wan no-preempt
set high-availability vrrp group wan hello-source-address {{ required "no private address" .Machine.InternalIP }}
{{- range .Peers }}
set high-availability vrrp group wan peer-address {{ .InternalIP }}
{{- end }}
set high-availability vrrp group wan transition-script master '/usr/local/bin/hcloud-vrrp-failover wan'
{{- if .Values.floatingIP }}
set interfaces dummy dum0 address {{ .Values.floatingIP }}/32
{{- end }}
{{- if .Values.own }}
# Something every router has of its own, by slot.
set interfaces dummy dum1 address {{ .Values.own }}
set interfaces dummy dum1 description 'slot {{ .Router.Slot }}'
{{- end }}

set firewall ipv4 input filter default-action drop
set firewall ipv4 input filter rule 10 action accept
set firewall ipv4 input filter rule 10 state established
set firewall ipv4 input filter rule 10 state related
set firewall ipv4 input filter rule 20 action accept
set firewall ipv4 input filter rule 20 protocol icmp
set firewall ipv4 input filter rule 30 action accept
set firewall ipv4 input filter rule 30 protocol tcp
set firewall ipv4 input filter rule 30 destination port 443
set firewall ipv4 input filter rule 35 action accept
set firewall ipv4 input filter rule 35 protocol tcp
set firewall ipv4 input filter rule 35 destination port 22
set firewall ipv4 input filter rule 50 action accept
set firewall ipv4 input filter rule 50 protocol vrrp
set firewall ipv4 input filter rule 50 inbound-interface name {{ .Host.PrivateInterface }}
`

func (s *suite) create() {
	s.t.Helper()
	networkCIDR := netip.MustParsePrefix(env(s.t, "E2E_NETWORK_CIDR"))
	labels := map[string]string{"router": "e2e"}
	serverImage := os.Getenv("E2E_SERVER_IMAGE")
	if serverImage == "" {
		serverImage = "ubuntu-24.04"
	}
	var sshKeys []string
	if key := os.Getenv("E2E_SSH_KEY"); key != "" {
		sshKeys = []string{key}
	}
	failover := `{"tokenFile":"/config/hetzner/token","groups":{"wan":{}}}`
	if s.floatingIP != "" {
		failover = `{"tokenFile":"/config/hetzner/token","groups":{"wan":{"floatingIPs":["` + s.floatingIP + `"]}}}`
	}

	objects := []client.Object{
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "hcloud", Namespace: s.namespace},
			Data:       map[string][]byte{"token": []byte(env(s.t, "HCLOUD_TOKEN"))},
		},
		&infrav1alpha1.HetznerRouterNetwork{
			ObjectMeta: metav1.ObjectMeta{Name: "e2e", Namespace: s.namespace},
			Spec: infrav1alpha1.HetznerRouterNetworkSpec{
				TokenSecretRef: infrav1alpha1.SecretKeyReference{Name: "hcloud"},
				Network:        infrav1alpha1.HetznerNetworkAttachment{Name: env(s.t, "E2E_NETWORK")},
				Firewall: infrav1alpha1.HetznerFirewall{
					ManagementSources: []infrav1alpha1.ManagementSource{{CIDR: env(s.t, "E2E_MANAGEMENT_CIDR")}},
					Rules: []infrav1alpha1.FirewallRule{
						{Description: "ping", Protocol: "icmp"},
						{Description: "ssh for debugging", Protocol: "tcp", Port: "22", SourceCIDRs: []string{env(s.t, "E2E_MANAGEMENT_CIDR")}},
					},
				},
				SSHKeys: sshKeys,
			},
		},
		&infrav1alpha1.HetznerMachineTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "e2e", Namespace: s.namespace},
			Spec: infrav1alpha1.HetznerMachineTemplateSpec{Template: infrav1alpha1.HetznerMachineTemplateResource{
				Spec: infrav1alpha1.HetznerMachineSpec{
					NetworkRef: corev1.LocalObjectReference{Name: "e2e"}, ServerType: "cx23", Location: "fsn1", Image: serverImage,
				},
			}},
		},
		&configv1alpha1.VyOSConfigTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "e2e", Namespace: s.namespace},
			Spec: configv1alpha1.VyOSConfigTemplateSpec{Template: configv1alpha1.VyOSConfigTemplateResource{
				Spec: configv1alpha1.VyOSConfigSpec{
					Image:    env(s.t, "E2E_VYOS_IMAGE"),
					Commands: commands,
					Values: []configv1alpha1.Value{
						{Name: "zone", ValueSource: configv1alpha1.ValueSource{Value: ptr.To("UTC")}},
						{Name: "network", ValueSource: configv1alpha1.ValueSource{Value: ptr.To(networkCIDR.String())}},
						{Name: "gateway", ValueSource: configv1alpha1.ValueSource{Value: ptr.To(networkCIDR.Addr().Next().String())}},
						{Name: "floatingIP", ValueSource: configv1alpha1.ValueSource{Value: ptr.To(s.floatingIP)}},
						{Name: "own", ValueSource: configv1alpha1.ValueSource{Value: ptr.To("")}},
						{Name: "token", ValueSource: configv1alpha1.ValueSource{SecretKeyRef: &corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{Name: "hcloud"}, Key: "token",
						}}},
					},
					Files: []configv1alpha1.File{
						{Path: "/config/hetzner/token", Permissions: "0600", ValueSource: configv1alpha1.ValueSource{Value: ptr.To("{{ .Values.token }}")}},
						{Path: "/config/hetzner/failover.json", Permissions: "0644", ValueSource: configv1alpha1.ValueSource{Value: ptr.To(failover)}},
					},
				},
			}},
		},
		&corev1alpha1.RouterDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: "e2e", Namespace: s.namespace},
			Spec: corev1alpha1.RouterDeploymentSpec{
				Replicas:        ptr.To(int32(2)),
				MinReadySeconds: ptr.To(int32(15)),
				Selector:        metav1.LabelSelector{MatchLabels: labels},
				Template: corev1alpha1.RouterTemplateSpec{
					ObjectMeta: corev1alpha1.ObjectMeta{Labels: labels},
					Spec: corev1alpha1.RouterTemplate{
						InfrastructureTemplateRef: corev1alpha1.ContractReference{APIGroup: infrav1alpha1.GroupVersion.Group, Kind: "HetznerMachineTemplate", Name: "e2e"},
						ConfigTemplateRef:         corev1alpha1.ContractReference{APIGroup: configv1alpha1.GroupVersion.Group, Kind: "VyOSConfigTemplate", Name: "e2e"},
					},
				},
			},
		},
	}
	for _, object := range objects {
		if s.slots() {
			switch typed := object.(type) {
			case *corev1alpha1.RouterDeployment:
				typed.Spec.Strategy.Type = corev1alpha1.SlotsStrategy
			case *infrav1alpha1.HetznerMachineTemplate:
				typed.Spec.Template.Spec.PrimaryIPv4BySlot = s.primaryIPs
			case *configv1alpha1.VyOSConfigTemplate:
				for slot := range s.primaryIPs {
					typed.Spec.Template.Spec.Slots = append(typed.Spec.Template.Spec.Slots, configv1alpha1.SlotSpec{Values: []configv1alpha1.Value{
						{Name: "own", ValueSource: configv1alpha1.ValueSource{Value: ptr.To(fmt.Sprintf("192.0.2.%d/32", 100+slot))}},
					}})
				}
			}
		}
		if err := k8s.Create(s.ctx, object); err != nil {
			s.t.Fatalf("create %T: %v", object, err)
		}
	}
}

func (s *suite) template() *configv1alpha1.VyOSConfigTemplate {
	s.t.Helper()
	template := &configv1alpha1.VyOSConfigTemplate{}
	if err := k8s.Get(s.ctx, types.NamespacedName{Namespace: s.namespace, Name: "e2e"}, template); err != nil {
		s.t.Fatal(err)
	}
	return template
}

// floatingIPServer returns the name of the server the Floating IP is
// assigned to, or "".
func (s *suite) floatingIPServer() string {
	s.t.Helper()
	all, err := s.hetzner.FloatingIP.All(s.ctx)
	if err != nil {
		s.t.Fatal(err)
	}
	for _, fip := range all {
		if fip.IP.String() != s.floatingIP || fip.Server == nil {
			continue
		}
		for name, server := range s.servers() {
			if server.ID == fip.Server.ID {
				return name
			}
		}
	}
	return ""
}

func TestRoutersOnHetzner(t *testing.T) {
	if restConfig == nil {
		t.Skip("HCLOUD_TOKEN and E2E_NETWORK are not set; this test creates billed servers, see `make e2e`")
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &suite{
		t: t, ctx: ctx, namespace: testenv.Namespace(t, k8s),
		hetzner:    hcloud.NewClient(hcloud.WithToken(env(t, "HCLOUD_TOKEN")), hcloud.WithApplication("router-api-e2e", "dev")),
		floatingIP: os.Getenv("E2E_FLOATING_IP"),
	}
	if names := os.Getenv("E2E_PRIMARY_IPS"); names != "" {
		s.primaryIPs = strings.Split(names, ",")
	}
	if leftovers := s.serverNames(); len(leftovers) > 0 {
		t.Fatalf("the project already has servers managed by router-api: %v", leftovers)
	}

	logger := slog.New(slog.DiscardHandler)
	if os.Getenv("E2E_LOG") != "" {
		logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	cfg := &manager.Config{MetricsAddress: "0", ProbeAddress: "0", Namespace: s.namespace, InstallCRDs: true}
	var wg sync.WaitGroup
	for _, definition := range []*manager.Definition{managers.Core(), managers.Hetzner(cloud.NewFactory()), managers.VyOS()} {
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
		if t.Failed() && os.Getenv("E2E_KEEP") != "" {
			// For looking at what went wrong on the servers themselves.
			t.Logf("E2E_KEEP is set: leaving %v at Hetzner. They are billed until deleted.", s.serverNames())
			return
		}
		s.purge()
	})

	s.create()

	// --- creation -----------------------------------------------------------
	began := time.Now()
	s.eventually("two routers to be ready", 15*time.Minute, func() bool { return len(s.ready()) == 2 }, nil)
	first := s.ready()
	s.logf("routers %v ready %s after the deployment was created", first, time.Since(began).Round(time.Second))
	if s.slots() {
		if problem := s.misplaced(); problem != "" {
			t.Fatal(problem)
		}
		s.logf("every router is at the Primary IP of its slot")
	}
	if got := s.serverNames(); !slices.Equal(got, first) {
		t.Fatalf("servers = %v, routers = %v", got, first)
	}
	if s.floatingIP != "" {
		s.eventually("the Floating IP to be assigned to a router", 3*time.Minute, func() bool { return s.floatingIPServer() != "" }, nil)
		s.logf("the Floating IP is on %s", s.floatingIPServer())
	}

	neverShort := func() string {
		if ready := s.ready(); len(ready) < 2 {
			return fmt.Sprintf("only %v are ready; the group must never drop below 2", ready)
		}
		return ""
	}
	// With slots the promise during a replacement is a different one: a
	// slot holds one router, so never more than two exist, and they go one
	// after the other, so never none is ready.
	duringReplacement := neverShort
	if s.slots() {
		duringReplacement = func() string {
			if n := len(s.routers()); n > 2 {
				return fmt.Sprintf("%d routers exist; a slot holds one", n)
			}
			if len(s.ready()) == 0 {
				return "no router is ready; they have to be replaced one after the other"
			}
			return ""
		}
	}
	var reachability *probe
	if s.floatingIP != "" {
		reachability = startProbe(s.floatingIP)
	}

	// --- a configuration change is applied in place -----------------------
	template := s.template()
	template.Spec.Template.Spec.Values[0].Value = ptr.To("Europe/Berlin")
	if err := k8s.Update(s.ctx, template); err != nil {
		t.Fatal(err)
	}
	s.eventually("both routers to run the changed configuration", 10*time.Minute, func() bool {
		for _, name := range first {
			router := &corev1alpha1.Router{}
			config := &configv1alpha1.VyOSConfig{}
			key := types.NamespacedName{Namespace: s.namespace, Name: name}
			if k8s.Get(s.ctx, key, router) != nil || k8s.Get(s.ctx, key, config) != nil {
				return false
			}
			zone := ""
			for _, value := range config.Spec.Values {
				if value.Name == "zone" {
					zone = ptr.Deref(value.Value, "")
				}
			}
			if zone != "Europe/Berlin" || !conditions.IsTrue(router.Status.Conditions, corev1alpha1.ConfigAppliedCondition) {
				return false
			}
		}
		return true
	}, neverShort)
	if got := s.serverNames(); !slices.Equal(got, first) {
		t.Fatalf("servers = %v, want the same %v: a configuration change must not replace servers", got, first)
	}

	// --- a change to the bootstrap data replaces the routers ---------------
	template = s.template()
	template.Spec.Template.Spec.Files = append(template.Spec.Template.Spec.Files, configv1alpha1.File{
		Path: "/config/e2e-marker", ValueSource: configv1alpha1.ValueSource{Value: ptr.To("second generation")},
	})
	if err := k8s.Update(s.ctx, template); err != nil {
		t.Fatal(err)
	}
	began = time.Now()
	s.eventually("both routers to be replaced", 25*time.Minute, func() bool {
		ready := s.ready()
		if len(ready) != 2 || len(s.serverNames()) != 2 {
			return false
		}
		for _, name := range ready {
			if slices.Contains(first, name) {
				return false
			}
		}
		return !s.slots() || s.misplaced() == ""
	}, duringReplacement)
	s.logf("replaced %v with %v in %s", first, s.ready(), time.Since(began).Round(time.Second))

	if reachability != nil {
		// Keep probing a little: the last old router has only just gone.
		time.Sleep(30 * time.Second)
		longest := reachability.finish()
		s.logf("longest interruption of the Floating IP during the configuration change and the replacement: %s", longest.Round(100*time.Millisecond))
		if longest > time.Minute {
			t.Errorf("the Floating IP was unreachable for %s", longest)
		}
		// The last old router is deleted the moment its replacement counts,
		// and the new master takes the address right after: give it that.
		s.eventually("the Floating IP to be on one of the new routers", 3*time.Minute, func() bool {
			return slices.Contains(s.ready(), s.floatingIPServer())
		}, nil)
	}

	// --- deletion leaves nothing behind -------------------------------------
	if err := k8s.Delete(s.ctx, &corev1alpha1.RouterDeployment{ObjectMeta: metav1.ObjectMeta{Name: "e2e", Namespace: s.namespace}}); err != nil {
		t.Fatal(err)
	}
	// envtest runs no garbage collector; the test stands in for it.
	if err := k8s.DeleteAllOf(s.ctx, &corev1alpha1.RouterSet{}, client.InNamespace(s.namespace)); err != nil {
		t.Fatal(err)
	}
	if err := k8s.DeleteAllOf(s.ctx, &corev1alpha1.Router{}, client.InNamespace(s.namespace)); err != nil {
		t.Fatal(err)
	}
	s.eventually("all servers to be deleted", 10*time.Minute, func() bool { return len(s.serverNames()) == 0 }, nil)
	s.eventually("the machine objects to be gone", 5*time.Minute, func() bool {
		machines := &infrav1alpha1.HetznerMachineList{}
		return k8s.List(s.ctx, machines, client.InNamespace(s.namespace)) == nil && len(machines.Items) == 0
	}, nil)
	if err := k8s.Delete(s.ctx, &infrav1alpha1.HetznerRouterNetwork{ObjectMeta: metav1.ObjectMeta{Name: "e2e", Namespace: s.namespace}}); err != nil {
		t.Fatal(err)
	}
	s.eventually("the firewall and placement group to be deleted", 5*time.Minute, func() bool {
		err := k8s.Get(s.ctx, types.NamespacedName{Namespace: s.namespace, Name: "e2e"}, &infrav1alpha1.HetznerRouterNetwork{})
		if !apierrors.IsNotFound(err) {
			return false
		}
		selector := hcloud.ListOpts{LabelSelector: hetznercontroller.ManagedByLabel + "=" + hetznercontroller.ManagedByValue}
		firewalls, err := s.hetzner.Firewall.AllWithOpts(s.ctx, hcloud.FirewallListOpts{ListOpts: selector})
		if err != nil || len(firewalls) != 0 {
			return false
		}
		groups, err := s.hetzner.PlacementGroup.AllWithOpts(s.ctx, hcloud.PlacementGroupListOpts{ListOpts: selector})
		return err == nil && len(groups) == 0
	}, nil)
}
