package controller

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1alpha1 "github.com/hauke-cloud/router-api/api/core/v1alpha1"
	infrav1alpha1 "github.com/hauke-cloud/router-api/api/infrastructure/v1alpha1"
	"github.com/hauke-cloud/router-api/internal/infrastructure/hetzner/cloud/fake"
	"github.com/hauke-cloud/router-api/internal/testenv"
)

var k8s client.Client

func TestMain(m *testing.M) {
	os.Exit(testenv.Run(m, func(c client.Client) { k8s = c }))
}

type fixture struct {
	t         *testing.T
	ctx       context.Context
	namespace string
	cloud     *fake.Cloud
	// dns is what host names resolve to; a missing name fails to resolve.
	dns     map[string][]string
	network *NetworkReconciler
	machine *MachineReconciler
}

// newFixture creates the token Secret and a HetznerRouterNetwork named net
// that attaches to the existing Hetzner network "lab".
func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{
		t: t, ctx: context.Background(), namespace: testenv.Namespace(t, k8s),
		cloud: fake.New("lab"),
		dns:   map[string][]string{"lab.example.net": {"198.51.100.7"}},
	}
	f.network = &NetworkReconciler{Client: k8s, Cloud: f.cloud.Factory(), Resolve: f.resolve}
	f.machine = &MachineReconciler{Client: k8s, Cloud: f.cloud.Factory()}

	objects := []client.Object{
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "hcloud", Namespace: f.namespace},
			Data:       map[string][]byte{"token": []byte("s3cret")},
		},
		&infrav1alpha1.HetznerRouterNetwork{
			ObjectMeta: metav1.ObjectMeta{Name: "net", Namespace: f.namespace},
			Spec: infrav1alpha1.HetznerRouterNetworkSpec{
				TokenSecretRef: infrav1alpha1.SecretKeyReference{Name: "hcloud"},
				Network:        infrav1alpha1.HetznerNetworkAttachment{Name: "lab"},
				Firewall: infrav1alpha1.HetznerFirewall{
					ManagementSources: []infrav1alpha1.ManagementSource{
						{CIDR: "192.0.2.0/24"},
						{Hostname: "lab.example.net"},
					},
					Rules: []infrav1alpha1.FirewallRule{
						{Description: "wireguard", Protocol: "udp", Port: "51820"},
						{Protocol: "icmp", SourceCIDRs: []string{"0.0.0.0/0"}},
					},
				},
				SSHKeys: []string{"yubikey"},
			},
		},
	}
	for _, object := range objects {
		if err := k8s.Create(f.ctx, object); err != nil {
			t.Fatalf("create %T: %v", object, err)
		}
	}
	return f
}

func (f *fixture) resolve(_ context.Context, host string) ([]netip.Addr, error) {
	answers, ok := f.dns[host]
	if !ok {
		return nil, errors.New("no such host")
	}
	addrs := make([]netip.Addr, len(answers))
	for i, answer := range answers {
		addrs[i] = netip.MustParseAddr(answer)
	}
	return addrs, nil
}

func (f *fixture) key(name string) types.NamespacedName {
	return types.NamespacedName{Namespace: f.namespace, Name: name}
}

func (f *fixture) reconcileNetwork() reconcile.Result {
	f.t.Helper()
	result, err := f.network.Reconcile(f.ctx, reconcile.Request{NamespacedName: f.key("net")})
	if err != nil {
		f.t.Fatalf("reconcile network: %v", err)
	}
	return result
}

func (f *fixture) reconcileMachine() reconcile.Result {
	f.t.Helper()
	result, err := f.machine.Reconcile(f.ctx, reconcile.Request{NamespacedName: f.key("edge")})
	if err != nil {
		f.t.Fatalf("reconcile machine: %v", err)
	}
	return result
}

func (f *fixture) net() *infrav1alpha1.HetznerRouterNetwork {
	f.t.Helper()
	network := &infrav1alpha1.HetznerRouterNetwork{}
	if err := k8s.Get(f.ctx, f.key("net"), network); err != nil {
		f.t.Fatal(err)
	}
	return network
}

func (f *fixture) hetznerMachine() *infrav1alpha1.HetznerMachine {
	f.t.Helper()
	machine := &infrav1alpha1.HetznerMachine{}
	if err := k8s.Get(f.ctx, f.key("edge"), machine); err != nil {
		f.t.Fatal(err)
	}
	return machine
}

// createMachine creates a RouterMachine named edge and the HetznerMachine it
// owns. With bootstrap, the machine already points at a bootstrap Secret.
func (f *fixture) createMachine(bootstrap bool) {
	f.t.Helper()
	owner := &corev1alpha1.RouterMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: f.namespace},
		Spec: corev1alpha1.RouterMachineSpec{InfrastructureRef: corev1alpha1.ContractReference{
			APIGroup: infrav1alpha1.GroupVersion.Group, Kind: "HetznerMachine", Name: "edge",
		}},
	}
	if bootstrap {
		owner.Spec.Bootstrap.DataSecretName = ptr.To("edge-bootstrap")
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "edge-bootstrap", Namespace: f.namespace},
			Data:       map[string][]byte{"value": []byte("#cloud-config\n")},
		}
		if err := k8s.Create(f.ctx, secret); err != nil {
			f.t.Fatal(err)
		}
	}
	if err := k8s.Create(f.ctx, owner); err != nil {
		f.t.Fatal(err)
	}
	machine := &infrav1alpha1.HetznerMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name: "edge", Namespace: f.namespace,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(owner, corev1alpha1.GroupVersion.WithKind("RouterMachine"))},
		},
		Spec: infrav1alpha1.HetznerMachineSpec{
			NetworkRef: corev1.LocalObjectReference{Name: "net"}, ServerType: "cx23", Location: "fsn1",
		},
	}
	if err := k8s.Create(f.ctx, machine); err != nil {
		f.t.Fatal(err)
	}
}
