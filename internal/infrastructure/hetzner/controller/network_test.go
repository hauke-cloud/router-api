package controller

import (
	"slices"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	corev1alpha1 "github.com/hauke-cloud/router-api/api/core/v1alpha1"
	infrav1alpha1 "github.com/hauke-cloud/router-api/api/infrastructure/v1alpha1"
	"github.com/hauke-cloud/router-api/internal/conditions"
	"github.com/hauke-cloud/router-api/internal/infrastructure/hetzner/cloud"
)

// ruleStrings renders rules as "protocol port source+source".
func ruleStrings(rules []cloud.FirewallRule) []string {
	out := make([]string, len(rules))
	for i, rule := range rules {
		sources := make([]string, len(rule.Sources))
		for j, source := range rule.Sources {
			sources[j] = source.String()
		}
		out[i] = rule.Protocol + " " + rule.Port + " " + strings.Join(sources, "+")
	}
	return out
}

func TestNetworkBecomesReady(t *testing.T) {
	f := newFixture(t)

	result := f.reconcileNetwork()

	network := f.net()
	if !conditions.IsTrue(network.Status.Conditions, corev1alpha1.ReadyCondition) {
		t.Fatalf("Ready = %+v", conditions.Get(network.Status.Conditions, corev1alpha1.ReadyCondition))
	}
	if !controllerutil.ContainsFinalizer(network, infrav1alpha1.HetznerRouterNetworkFinalizer) {
		t.Error("no finalizer")
	}
	if network.Status.NetworkID == 0 || network.Status.FirewallID == 0 || network.Status.PlacementGroupID == 0 {
		t.Errorf("status = %+v", network.Status)
	}
	if len(f.cloud.Tokens) == 0 || f.cloud.Tokens[0] != "s3cret" {
		t.Errorf("tokens = %v", f.cloud.Tokens)
	}

	// The host name was resolved, and its address is allowed in next to the
	// fixed range.
	want := []string{"192.0.2.0/24", "198.51.100.7/32"}
	if !slices.Equal(network.Status.ManagementCIDRs, want) {
		t.Errorf("managementCIDRs = %v, want %v", network.Status.ManagementCIDRs, want)
	}

	firewall := f.cloud.Firewall(resourceName(network))
	if firewall == nil {
		t.Fatal("no firewall was created")
	}
	wantRules := []string{
		"tcp 443 192.0.2.0/24+198.51.100.7/32",
		"udp 51820 0.0.0.0/0+::/0",
		"icmp  0.0.0.0/0",
	}
	if got := ruleStrings(firewall.Rules); !slices.Equal(got, wantRules) {
		t.Errorf("rules = %q, want %q", got, wantRules)
	}

	// A host name is only good until its address changes.
	if result.RequeueAfter == 0 || result.RequeueAfter > ResolveInterval {
		t.Errorf("RequeueAfter = %v", result.RequeueAfter)
	}
}

func TestFirewallFollowsTheHostName(t *testing.T) {
	f := newFixture(t)
	f.reconcileNetwork()

	// The lab's address rotates.
	f.dns["lab.example.net"] = []string{"198.51.100.99", "2001:db8::99"}
	f.reconcileNetwork()

	network := f.net()
	want := []string{"192.0.2.0/24", "198.51.100.99/32", "2001:db8::99/128"}
	if !slices.Equal(network.Status.ManagementCIDRs, want) {
		t.Errorf("managementCIDRs = %v, want %v", network.Status.ManagementCIDRs, want)
	}
	rules := f.cloud.Firewall(resourceName(network)).Rules
	if got := len(rules[0].Sources); got != 3 {
		t.Errorf("management rule has %d sources, want 3", got)
	}
}

func TestResolutionFailureKeepsTheLastAnswer(t *testing.T) {
	f := newFixture(t)
	f.reconcileNetwork()

	delete(f.dns, "lab.example.net")
	f.reconcileNetwork()

	network := f.net()
	// Dropping the address would lock the operator out the moment its DNS
	// server hiccups. The last known address stays until there is a new one.
	want := []string{"192.0.2.0/24", "198.51.100.7/32"}
	if !slices.Equal(network.Status.ManagementCIDRs, want) {
		t.Errorf("managementCIDRs = %v, want %v", network.Status.ManagementCIDRs, want)
	}
	if !conditions.IsTrue(network.Status.Conditions, corev1alpha1.ReadyCondition) {
		t.Error("Ready went False: routers would stop being created over a DNS hiccup")
	}
	resolved := conditions.Get(network.Status.Conditions, ManagementSourcesResolvedCondition)
	if resolved == nil || resolved.Status != metav1.ConditionFalse {
		t.Errorf("%s = %+v, want False so that the failure is visible", ManagementSourcesResolvedCondition, resolved)
	}
}

func TestNetworkWaitsForItsPrerequisites(t *testing.T) {
	t.Run("token", func(t *testing.T) {
		f := newFixture(t)
		network := f.net()
		network.Spec.TokenSecretRef.Name = "missing"
		if err := k8s.Update(f.ctx, network); err != nil {
			t.Fatal(err)
		}
		f.reconcileNetwork()
		ready := conditions.Get(f.net().Status.Conditions, corev1alpha1.ReadyCondition)
		if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != ReasonTokenUnavailable {
			t.Errorf("Ready = %+v", ready)
		}
	})
	t.Run("network", func(t *testing.T) {
		f := newFixture(t)
		network := f.net()
		network.Spec.Network.Name = "nope"
		if err := k8s.Update(f.ctx, network); err != nil {
			t.Fatal(err)
		}
		f.reconcileNetwork()
		ready := conditions.Get(f.net().Status.Conditions, corev1alpha1.ReadyCondition)
		if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != ReasonNetworkNotFound {
			t.Errorf("Ready = %+v", ready)
		}
		if f.cloud.Firewall(resourceName(f.net())) != nil {
			t.Error("a firewall was created for a network that does not exist")
		}
	})
}

func TestPlacementGroupIsOptional(t *testing.T) {
	f := newFixture(t)
	network := f.net()
	network.Spec.PlacementGroup.Enabled = ptr.To(false)
	if err := k8s.Update(f.ctx, network); err != nil {
		t.Fatal(err)
	}

	f.reconcileNetwork()

	if got := f.cloud.PlacementGroups(); len(got) != 0 {
		t.Errorf("placement groups = %v", got)
	}
	if f.net().Status.PlacementGroupID != 0 {
		t.Error("status reports a placement group")
	}
}

func TestNetworkDeletionWaitsForServers(t *testing.T) {
	f := newFixture(t)
	f.reconcileNetwork()
	f.createMachine(true)
	f.reconcileMachine()
	name := resourceName(f.net())

	if err := k8s.Delete(f.ctx, f.net()); err != nil {
		t.Fatal(err)
	}
	f.reconcileNetwork()

	// Still attached to a server: Hetzner would refuse, and the routers
	// would be left without the object that holds their token.
	if f.cloud.Firewall(name) == nil {
		t.Fatal("the firewall was deleted while a machine still uses the network")
	}
	if !controllerutil.ContainsFinalizer(f.net(), infrav1alpha1.HetznerRouterNetworkFinalizer) {
		t.Fatal("the finalizer was removed while a machine still uses the network")
	}

	if err := k8s.Delete(f.ctx, f.hetznerMachine()); err != nil {
		t.Fatal(err)
	}
	f.reconcileMachine()
	f.reconcileNetwork()

	if f.cloud.Firewall(name) != nil || len(f.cloud.PlacementGroups()) != 0 {
		t.Error("the firewall or the placement group was left behind")
	}
	if err := k8s.Get(f.ctx, f.key("net"), &infrav1alpha1.HetznerRouterNetwork{}); !apierrors.IsNotFound(err) {
		t.Errorf("the network object still exists: %v", err)
	}
}

// expose creates a RouterExposure named edge, makes the network's firewall
// follow it and plays the core controller: the given ports are its status.
func (f *fixture) expose(ports ...corev1alpha1.ExposedPort) {
	f.t.Helper()
	exposure := &corev1alpha1.RouterExposure{ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: f.namespace}}
	if err := k8s.Create(f.ctx, exposure); err != nil {
		f.t.Fatal(err)
	}
	if ports != nil {
		exposure.Status.Ports = ports
		exposure.Status.ObservedGeneration = exposure.Generation
		if err := k8s.Status().Update(f.ctx, exposure); err != nil {
			f.t.Fatal(err)
		}
	}
	network := f.net()
	network.Spec.Firewall.ExposureRef = &corev1alpha1.LocalObjectReference{Name: "edge"}
	if err := k8s.Update(f.ctx, network); err != nil {
		f.t.Fatal(err)
	}
}

func TestFirewallFollowsTheExposure(t *testing.T) {
	f := newFixture(t)
	f.expose(
		corev1alpha1.ExposedPort{Protocol: corev1alpha1.ExposedTCP, Port: 80},
		corev1alpha1.ExposedPort{Protocol: corev1alpha1.ExposedTCP, Port: 443},
		corev1alpha1.ExposedPort{Protocol: corev1alpha1.ExposedTCP, Port: 8000},
		corev1alpha1.ExposedPort{Protocol: corev1alpha1.ExposedTCP, Port: 8001},
		corev1alpha1.ExposedPort{Protocol: corev1alpha1.ExposedTCP, Port: 8002},
		corev1alpha1.ExposedPort{Protocol: corev1alpha1.ExposedUDP, Port: 53},
	)

	f.reconcileNetwork()

	network := f.net()
	// The management rule and the configured rules first, as before, then
	// the ports of the Gateways, a run of them as one range.
	want := []string{
		"tcp 443 192.0.2.0/24+198.51.100.7/32",
		"udp 51820 0.0.0.0/0+::/0",
		"icmp  0.0.0.0/0",
		"tcp 80 0.0.0.0/0+::/0",
		"tcp 443 0.0.0.0/0+::/0",
		"tcp 8000-8002 0.0.0.0/0+::/0",
		"udp 53 0.0.0.0/0+::/0",
	}
	if got := ruleStrings(f.cloud.Firewall(resourceName(network)).Rules); !slices.Equal(got, want) {
		t.Errorf("rules = %q, want %q", got, want)
	}
	if !conditions.IsTrue(network.Status.Conditions, ExposureAppliedCondition) {
		t.Errorf("%s = %+v", ExposureAppliedCondition, conditions.Get(network.Status.Conditions, ExposureAppliedCondition))
	}

	// A Gateway is gone: its port closes.
	exposure := &corev1alpha1.RouterExposure{}
	if err := k8s.Get(f.ctx, f.key("edge"), exposure); err != nil {
		t.Fatal(err)
	}
	exposure.Status.Ports = exposure.Status.Ports[:2]
	if err := k8s.Status().Update(f.ctx, exposure); err != nil {
		t.Fatal(err)
	}
	f.reconcileNetwork()
	if got := ruleStrings(f.cloud.Firewall(resourceName(network)).Rules); !slices.Equal(got, want[:5]) {
		t.Errorf("rules = %q, want %q", got, want[:5])
	}
}

// The operator's own way in, and what was configured by hand, must not
// depend on a list of Gateways being there.
func TestFirewallWithoutAnExposureToFollow(t *testing.T) {
	static := []string{
		"tcp 443 192.0.2.0/24+198.51.100.7/32",
		"udp 51820 0.0.0.0/0+::/0",
		"icmp  0.0.0.0/0",
	}
	t.Run("missing", func(t *testing.T) {
		f := newFixture(t)
		network := f.net()
		network.Spec.Firewall.ExposureRef = &corev1alpha1.LocalObjectReference{Name: "nope"}
		if err := k8s.Update(f.ctx, network); err != nil {
			t.Fatal(err)
		}
		f.reconcileNetwork()

		network = f.net()
		if got := ruleStrings(f.cloud.Firewall(resourceName(network)).Rules); !slices.Equal(got, static) {
			t.Errorf("rules = %q, want %q", got, static)
		}
		applied := conditions.Get(network.Status.Conditions, ExposureAppliedCondition)
		if applied == nil || applied.Status != metav1.ConditionFalse || applied.Reason != ReasonExposureNotFound {
			t.Errorf("%s = %+v", ExposureAppliedCondition, applied)
		}
		if !conditions.IsTrue(network.Status.Conditions, corev1alpha1.ReadyCondition) {
			t.Error("Ready went False: routers would stop being created over a missing list of Gateways")
		}
	})
	t.Run("not computed yet", func(t *testing.T) {
		f := newFixture(t)
		f.expose()
		f.reconcileNetwork()

		network := f.net()
		if got := ruleStrings(f.cloud.Firewall(resourceName(network)).Rules); !slices.Equal(got, static) {
			t.Errorf("rules = %q, want %q", got, static)
		}
		applied := conditions.Get(network.Status.Conditions, ExposureAppliedCondition)
		if applied == nil || applied.Status != metav1.ConditionFalse || applied.Reason != ReasonExposureNotObserved {
			t.Errorf("%s = %+v", ExposureAppliedCondition, applied)
		}
	})
}
