package exposure

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	corev1alpha1 "github.com/hauke-cloud/router-api/api/core/v1alpha1"
	"github.com/hauke-cloud/router-api/internal/conditions"
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
	r         *Reconciler
}

// newFixture creates a RouterExposure named edge that listens to its own
// namespace and to the given ones.
func newFixture(t *testing.T, namespaces ...string) *fixture {
	t.Helper()
	f := &fixture{t: t, ctx: context.Background(), namespace: testenv.Namespace(t, k8s), r: &Reconciler{Client: k8s}}
	exposure := &corev1alpha1.RouterExposure{
		ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: f.namespace},
		Spec:       corev1alpha1.RouterExposureSpec{Gateways: corev1alpha1.GatewaySource{Namespaces: namespaces}},
	}
	if err := k8s.Create(f.ctx, exposure); err != nil {
		t.Fatal(err)
	}
	return f
}

type listener struct {
	protocol gatewayv1.ProtocolType
	port     int32
}

// gateway creates a Gateway that names the fixture's RouterExposure and has
// been given the addresses, as its implementation would report them.
func (f *fixture) gateway(namespace, name string, listeners []listener, addresses ...string) *gatewayv1.Gateway {
	f.t.Helper()
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: namespace,
			Annotations: map[string]string{corev1alpha1.ExposeAnnotation: f.namespace + "/edge"},
		},
		Spec: gatewayv1.GatewaySpec{GatewayClassName: "test"},
	}
	for i, l := range listeners {
		spec := gatewayv1.Listener{Name: gatewayv1.SectionName(fmt.Sprintf("l%d", i)), Protocol: l.protocol, Port: l.port}
		if l.protocol == gatewayv1.TLSProtocolType {
			spec.TLS = &gatewayv1.ListenerTLSConfig{Mode: ptr.To(gatewayv1.TLSModePassthrough)}
		}
		gateway.Spec.Listeners = append(gateway.Spec.Listeners, spec)
	}
	if err := k8s.Create(f.ctx, gateway); err != nil {
		f.t.Fatalf("create Gateway: %v", err)
	}
	f.address(gateway, addresses...)
	return gateway
}

func (f *fixture) address(gateway *gatewayv1.Gateway, addresses ...string) {
	f.t.Helper()
	gateway.Status.Addresses = nil
	for _, address := range addresses {
		gateway.Status.Addresses = append(gateway.Status.Addresses, gatewayv1.GatewayStatusAddress{Value: address})
	}
	if err := k8s.Status().Update(f.ctx, gateway); err != nil {
		f.t.Fatalf("update Gateway status: %v", err)
	}
}

func (f *fixture) reconcile() {
	f.t.Helper()
	if _, err := f.r.Reconcile(f.ctx, reconcile.Request{NamespacedName: client.ObjectKey{Namespace: f.namespace, Name: "edge"}}); err != nil {
		f.t.Fatalf("Reconcile: %v", err)
	}
}

func (f *fixture) status() *corev1alpha1.RouterExposureStatus {
	f.t.Helper()
	exposure := &corev1alpha1.RouterExposure{}
	if err := k8s.Get(f.ctx, client.ObjectKey{Namespace: f.namespace, Name: "edge"}, exposure); err != nil {
		f.t.Fatal(err)
	}
	return &exposure.Status
}

// endpointStrings renders endpoints as "address tcp/80 udp/53".
func endpointStrings(endpoints []corev1alpha1.ExposedEndpoint) []string {
	out := make([]string, len(endpoints))
	for i, endpoint := range endpoints {
		out[i] = endpoint.Address + " " + portStrings(endpoint.Ports)
	}
	return out
}

func portStrings(ports []corev1alpha1.ExposedPort) string {
	out := make([]string, len(ports))
	for i, port := range ports {
		out[i] = fmt.Sprintf("%s/%d", port.Protocol, port.Port)
	}
	return strings.Join(out, " ")
}

func TestCollectsListenersOfAnnotatedGateways(t *testing.T) {
	f := newFixture(t)
	f.gateway(f.namespace, "web", []listener{{gatewayv1.HTTPSProtocolType, 443}, {gatewayv1.HTTPProtocolType, 80}}, "203.0.113.18")
	f.gateway(f.namespace, "dns", []listener{{gatewayv1.UDPProtocolType, 53}, {gatewayv1.TCPProtocolType, 53}, {gatewayv1.TLSProtocolType, 853}},
		"203.0.113.17", "2001:db8::17")
	// Not annotated: none of this controller's business.
	plain := f.gateway(f.namespace, "internal", []listener{{gatewayv1.HTTPProtocolType, 8080}}, "203.0.113.19")
	plain.Annotations = nil
	if err := k8s.Update(f.ctx, plain); err != nil {
		t.Fatal(err)
	}

	f.reconcile()

	status := f.status()
	want := []string{
		"203.0.113.17 tcp/53 tcp/853 udp/53",
		"203.0.113.18 tcp/80 tcp/443",
		"2001:db8::17 tcp/53 tcp/853 udp/53",
	}
	if got := endpointStrings(status.Endpoints); !slices.Equal(got, want) {
		t.Errorf("endpoints = %q, want %q", got, want)
	}
	if got, want := portStrings(status.Ports), "tcp/53 tcp/80 tcp/443 tcp/853 udp/53"; got != want {
		t.Errorf("ports = %q, want %q", got, want)
	}
	if !conditions.IsTrue(status.Conditions, corev1alpha1.ReadyCondition) || status.ObservedGeneration == 0 {
		t.Errorf("status = %+v", status)
	}
	if len(status.Gateways) != 2 || !status.Gateways[0].Exposed || !status.Gateways[1].Exposed {
		t.Errorf("gateways = %+v", status.Gateways)
	}
}

func TestGatewaysSharingAnAddressAreMerged(t *testing.T) {
	f := newFixture(t)
	f.gateway(f.namespace, "a", []listener{{gatewayv1.HTTPSProtocolType, 443}}, "203.0.113.17")
	f.gateway(f.namespace, "b", []listener{{gatewayv1.HTTPSProtocolType, 443}, {gatewayv1.TCPProtocolType, 5432}}, "203.0.113.17")

	f.reconcile()

	want := []string{"203.0.113.17 tcp/443 tcp/5432"}
	if got := endpointStrings(f.status().Endpoints); !slices.Equal(got, want) {
		t.Errorf("endpoints = %q, want %q", got, want)
	}
}

// Whoever can create a Gateway somewhere must not be able to open ports on
// the routers with it.
func TestAGatewayInANamespaceThatIsNotListedIsIgnored(t *testing.T) {
	f := newFixture(t)
	elsewhere := testenv.Namespace(t, k8s)
	f.gateway(elsewhere, "sneaky", []listener{{gatewayv1.TCPProtocolType, 22}}, "203.0.113.20")

	f.reconcile()

	status := f.status()
	if len(status.Endpoints) != 0 || len(status.Ports) != 0 {
		t.Errorf("endpoints = %+v, ports = %+v", status.Endpoints, status.Ports)
	}
	if len(status.Gateways) != 1 || status.Gateways[0].Exposed || !strings.Contains(status.Gateways[0].Message, elsewhere) {
		t.Errorf("gateways = %+v, want the refusal to be visible", status.Gateways)
	}
}

func TestNamespacesCanBeListed(t *testing.T) {
	for name, list := range map[string]func(string) []string{
		"by name":  func(namespace string) []string { return []string{namespace} },
		"wildcard": func(string) []string { return []string{corev1alpha1.AllNamespaces} },
	} {
		t.Run(name, func(t *testing.T) {
			elsewhere := testenv.Namespace(t, k8s)
			f := newFixture(t, list(elsewhere)...)
			f.gateway(elsewhere, "web", []listener{{gatewayv1.HTTPSProtocolType, 443}}, "203.0.113.18")

			f.reconcile()

			want := []string{"203.0.113.18 tcp/443"}
			if got := endpointStrings(f.status().Endpoints); !slices.Equal(got, want) {
				t.Errorf("endpoints = %q, want %q", got, want)
			}
		})
	}
}

func TestAGatewayWithoutAnAddressWaits(t *testing.T) {
	f := newFixture(t)
	gateway := f.gateway(f.namespace, "web", []listener{{gatewayv1.HTTPSProtocolType, 443}})

	f.reconcile()
	status := f.status()
	if len(status.Endpoints) != 0 || status.Gateways[0].Exposed || !strings.Contains(status.Gateways[0].Message, "address") {
		t.Errorf("status = %+v", status)
	}

	f.address(gateway, "203.0.113.18")
	f.reconcile()
	if got := endpointStrings(f.status().Endpoints); !slices.Equal(got, []string{"203.0.113.18 tcp/443"}) {
		t.Errorf("endpoints = %q", got)
	}
}

func TestWhatCannotBeExposedIsReported(t *testing.T) {
	f := newFixture(t)
	gateway := f.gateway(f.namespace, "odd", []listener{{"example.net/quic", 443}, {gatewayv1.HTTPSProtocolType, 443}})
	gateway.Status.Addresses = []gatewayv1.GatewayStatusAddress{
		{Type: ptr.To(gatewayv1.HostnameAddressType), Value: "gw.example.net"},
		{Type: ptr.To(gatewayv1.IPAddressType), Value: "203.0.113.18"},
	}
	if err := k8s.Status().Update(f.ctx, gateway); err != nil {
		t.Fatal(err)
	}

	f.reconcile()

	status := f.status()
	if got := endpointStrings(status.Endpoints); !slices.Equal(got, []string{"203.0.113.18 tcp/443"}) {
		t.Errorf("endpoints = %q", got)
	}
	if !status.Gateways[0].Exposed || !strings.Contains(status.Gateways[0].Message, "example.net/quic") {
		t.Errorf("gateways = %+v", status.Gateways)
	}
}

func TestAGatewayThatLeavesIsNoLongerExposed(t *testing.T) {
	f := newFixture(t)
	gateway := f.gateway(f.namespace, "web", []listener{{gatewayv1.HTTPSProtocolType, 443}}, "203.0.113.18")
	f.reconcile()
	if len(f.status().Endpoints) != 1 {
		t.Fatalf("endpoints = %+v", f.status().Endpoints)
	}

	if err := k8s.Delete(f.ctx, gateway); err != nil {
		t.Fatal(err)
	}
	f.reconcile()

	status := f.status()
	if len(status.Endpoints) != 0 || len(status.Ports) != 0 || len(status.Gateways) != 0 {
		t.Errorf("status = %+v", status)
	}
}

// A cluster without the Gateway API, or one whose CRD is being replaced:
// what was exposed stays, and the condition says why nothing moves.
func TestWithoutTheGatewayAPINothingIsClosed(t *testing.T) {
	f := newFixture(t)
	f.gateway(f.namespace, "web", []listener{{gatewayv1.HTTPSProtocolType, 443}}, "203.0.113.18")
	f.reconcile()

	f.r.Client = without{k8s, "Gateway"}
	f.reconcile()

	status := f.status()
	if got := endpointStrings(status.Endpoints); !slices.Equal(got, []string{"203.0.113.18 tcp/443"}) {
		t.Errorf("endpoints = %q, want them kept", got)
	}
	ready := conditions.Get(status.Conditions, corev1alpha1.ReadyCondition)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != ReasonGatewayAPIUnavailable {
		t.Errorf("Ready = %+v", ready)
	}
}

// without answers a list of one kind of the Gateway API the way a cluster
// that does not serve it does.
type without struct {
	client.Client
	kind string
}

func (c without) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	var kind string
	switch list.(type) {
	case *gatewayv1.GatewayList:
		kind = "Gateway"
	case *gatewayv1.ListenerSetList:
		kind = "ListenerSet"
	}
	if kind == c.kind {
		return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: gatewayv1.GroupName, Kind: kind}}
	}
	return c.Client.List(ctx, list, opts...)
}

// listenerSet creates a ListenerSet attached to the Gateway, which its
// implementation has not looked at yet.
func (f *fixture) listenerSet(namespace, name string, gateway *gatewayv1.Gateway, listeners ...listener) *gatewayv1.ListenerSet {
	f.t.Helper()
	set := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: gatewayv1.ListenerSetSpec{ParentRef: gatewayv1.ParentGatewayReference{
			Name: gatewayv1.ObjectName(gateway.Name), Namespace: ptr.To(gatewayv1.Namespace(gateway.Namespace)),
		}},
	}
	for i, l := range listeners {
		set.Spec.Listeners = append(set.Spec.Listeners, gatewayv1.ListenerEntry{
			Name: gatewayv1.SectionName(fmt.Sprintf("l%d", i)), Protocol: l.protocol, Port: l.port,
		})
	}
	if err := k8s.Create(f.ctx, set); err != nil {
		f.t.Fatalf("create ListenerSet: %v", err)
	}
	return set
}

// accept answers a ListenerSet the way the Gateway's implementation does.
func (f *fixture) accept(set *gatewayv1.ListenerSet, status metav1.ConditionStatus, reason gatewayv1.ListenerSetConditionReason) {
	f.t.Helper()
	meta.SetStatusCondition(&set.Status.Conditions, metav1.Condition{
		Type: string(gatewayv1.ListenerSetConditionAccepted), Status: status, Reason: string(reason), ObservedGeneration: set.Generation,
	})
	if err := k8s.Status().Update(f.ctx, set); err != nil {
		f.t.Fatalf("update ListenerSet status: %v", err)
	}
}

func TestListenersOfAcceptedListenerSetsAreTheGateways(t *testing.T) {
	// The ListenerSet's namespace is the Gateway's to allow, not the
	// RouterExposure's: it is not listed here.
	f := newFixture(t)
	apps := testenv.Namespace(t, k8s)
	gateway := f.gateway(f.namespace, "web", []listener{{gatewayv1.HTTPSProtocolType, 443}}, "203.0.113.18", "2001:db8::18")
	f.gateway(f.namespace, "other", []listener{{gatewayv1.HTTPProtocolType, 80}}, "203.0.113.19")
	set := f.listenerSet(apps, "mail", gateway, listener{gatewayv1.TCPProtocolType, 25}, listener{gatewayv1.UDPProtocolType, 4500}, listener{"example.net/quic", 444})
	f.accept(set, metav1.ConditionTrue, gatewayv1.ListenerSetReasonAccepted)

	f.reconcile()

	status := f.status()
	want := []string{
		"203.0.113.18 tcp/25 tcp/443 udp/4500",
		"203.0.113.19 tcp/80",
		"2001:db8::18 tcp/25 tcp/443 udp/4500",
	}
	if got := endpointStrings(status.Endpoints); !slices.Equal(got, want) {
		t.Errorf("endpoints = %q, want %q", got, want)
	}
	if len(status.ListenerSets) != 1 {
		t.Fatalf("listenerSets = %+v", status.ListenerSets)
	}
	report := status.ListenerSets[0]
	if report.Namespace != apps || report.Name != "mail" || report.Gateway != f.namespace+"/web" || !report.Exposed ||
		!strings.Contains(report.Message, "example.net/quic") {
		t.Errorf("listenerSets = %+v", report)
	}
}

// Creating a ListenerSet that points at an exposed Gateway opens nothing. The
// Gateway has to take it.
func TestAListenerSetTheGatewayHasNotAcceptedIsIgnored(t *testing.T) {
	f := newFixture(t)
	gateway := f.gateway(f.namespace, "web", []listener{{gatewayv1.HTTPSProtocolType, 443}}, "203.0.113.18")
	set := f.listenerSet(f.namespace, "sneaky", gateway, listener{gatewayv1.TCPProtocolType, 22})

	for _, step := range []struct {
		status metav1.ConditionStatus
		reason gatewayv1.ListenerSetConditionReason
	}{
		// As the API server creates it, before anything looked at it.
		{},
		{metav1.ConditionFalse, gatewayv1.ListenerSetReasonNotAllowed},
	} {
		if step.status != "" {
			f.accept(set, step.status, step.reason)
		}
		f.reconcile()
		status := f.status()
		if got := endpointStrings(status.Endpoints); !slices.Equal(got, []string{"203.0.113.18 tcp/443"}) {
			t.Errorf("endpoints = %q", got)
		}
		if len(status.ListenerSets) != 1 || status.ListenerSets[0].Exposed || !strings.Contains(status.ListenerSets[0].Message, "not accepted") {
			t.Errorf("listenerSets = %+v, want the refusal to be visible", status.ListenerSets)
		}
	}
	if message := f.status().ListenerSets[0].Message; !strings.Contains(message, string(gatewayv1.ListenerSetReasonNotAllowed)) {
		t.Errorf("message = %q, want the Gateway's reason", message)
	}

	f.accept(set, metav1.ConditionTrue, gatewayv1.ListenerSetReasonAccepted)
	f.reconcile()
	if got := endpointStrings(f.status().Endpoints); !slices.Equal(got, []string{"203.0.113.18 tcp/22 tcp/443"}) {
		t.Errorf("endpoints = %q", got)
	}

	if err := k8s.Delete(f.ctx, set); err != nil {
		t.Fatal(err)
	}
	f.reconcile()
	status := f.status()
	if got := endpointStrings(status.Endpoints); !slices.Equal(got, []string{"203.0.113.18 tcp/443"}) || len(status.ListenerSets) != 0 {
		t.Errorf("endpoints = %q, listenerSets = %+v", got, status.ListenerSets)
	}
}

// An accepted ListenerSet does not get a Gateway past the RouterExposure.
func TestAListenerSetOfAGatewayThatMayNotAskIsIgnored(t *testing.T) {
	f := newFixture(t)
	elsewhere := testenv.Namespace(t, k8s)
	gateway := f.gateway(elsewhere, "sneaky", []listener{{gatewayv1.TCPProtocolType, 22}}, "203.0.113.20")
	// In the RouterExposure's own namespace, even.
	set := f.listenerSet(f.namespace, "more", gateway, listener{gatewayv1.TCPProtocolType, 23})
	f.accept(set, metav1.ConditionTrue, gatewayv1.ListenerSetReasonAccepted)
	// Attached to a Gateway that did not ask at all.
	plain := f.gateway(f.namespace, "internal", []listener{{gatewayv1.HTTPProtocolType, 8080}}, "203.0.113.19")
	plain.Annotations = nil
	if err := k8s.Update(f.ctx, plain); err != nil {
		t.Fatal(err)
	}
	f.accept(f.listenerSet(f.namespace, "internal", plain, listener{gatewayv1.TCPProtocolType, 24}), metav1.ConditionTrue, gatewayv1.ListenerSetReasonAccepted)

	f.reconcile()

	status := f.status()
	if len(status.Endpoints) != 0 || len(status.Ports) != 0 {
		t.Errorf("endpoints = %+v, ports = %+v", status.Endpoints, status.Ports)
	}
	if len(status.ListenerSets) != 1 || status.ListenerSets[0].Name != "more" || status.ListenerSets[0].Exposed ||
		!strings.Contains(status.ListenerSets[0].Message, elsewhere) {
		t.Errorf("listenerSets = %+v", status.ListenerSets)
	}
}

func TestAListenerSetOfAGatewayWithoutAnAddressWaits(t *testing.T) {
	f := newFixture(t)
	gateway := f.gateway(f.namespace, "web", []listener{{gatewayv1.HTTPSProtocolType, 443}})
	f.accept(f.listenerSet(f.namespace, "mail", gateway, listener{gatewayv1.TCPProtocolType, 25}), metav1.ConditionTrue, gatewayv1.ListenerSetReasonAccepted)

	f.reconcile()

	status := f.status()
	if len(status.Endpoints) != 0 || status.ListenerSets[0].Exposed || !strings.Contains(status.ListenerSets[0].Message, "address") {
		t.Errorf("status = %+v", status)
	}
}

// A Gateway API from before ListenerSets is all Gateways. One that had them
// and lost the kind is a CRD being replaced, and nothing is closed over it.
func TestWithoutListenerSets(t *testing.T) {
	f := newFixture(t)
	gateway := f.gateway(f.namespace, "web", []listener{{gatewayv1.HTTPSProtocolType, 443}}, "203.0.113.18")
	f.r.Client = without{k8s, "ListenerSet"}
	f.reconcile()

	status := f.status()
	if got := endpointStrings(status.Endpoints); !slices.Equal(got, []string{"203.0.113.18 tcp/443"}) {
		t.Errorf("endpoints = %q", got)
	}
	if !conditions.IsTrue(status.Conditions, corev1alpha1.ReadyCondition) {
		t.Errorf("conditions = %+v", status.Conditions)
	}

	f.r.Client = k8s
	f.accept(f.listenerSet(f.namespace, "mail", gateway, listener{gatewayv1.TCPProtocolType, 25}), metav1.ConditionTrue, gatewayv1.ListenerSetReasonAccepted)
	f.reconcile()
	f.r.Client = without{k8s, "ListenerSet"}
	f.reconcile()

	status = f.status()
	if got := endpointStrings(status.Endpoints); !slices.Equal(got, []string{"203.0.113.18 tcp/25 tcp/443"}) {
		t.Errorf("endpoints = %q, want them kept", got)
	}
	ready := conditions.Get(status.Conditions, corev1alpha1.ReadyCondition)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != ReasonGatewayAPIUnavailable {
		t.Errorf("Ready = %+v", ready)
	}
}

func TestParent(t *testing.T) {
	for want, ref := range map[string]gatewayv1.ParentGatewayReference{
		"apps/web":    {Name: "web"},
		"ingress/web": {Name: "web", Namespace: ptr.To(gatewayv1.Namespace("ingress")), Group: ptr.To(gatewayv1.Group(gatewayv1.GroupName)), Kind: ptr.To(gatewayv1.Kind("Gateway"))},
		"":            {Name: "web", Kind: ptr.To(gatewayv1.Kind("Service"))},
	} {
		set := &gatewayv1.ListenerSet{ObjectMeta: metav1.ObjectMeta{Namespace: "apps"}, Spec: gatewayv1.ListenerSetSpec{ParentRef: ref}}
		got := ""
		if key, ok := parent(set); ok {
			got = key.String()
		}
		if got != want {
			t.Errorf("parent(%+v) = %q, want %q", ref, got, want)
		}
	}
}

func TestTarget(t *testing.T) {
	for value, want := range map[string]string{
		"routers/edge": "routers/edge",
		"edge":         "apps/edge",
		"":             "",
		"/edge":        "",
		"routers/":     "",
	} {
		gateway := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{
			Namespace: "apps", Annotations: map[string]string{corev1alpha1.ExposeAnnotation: value},
		}}
		got := ""
		if key, ok := target(gateway); ok {
			got = key.String()
		}
		if got != want {
			t.Errorf("target(%q) = %q, want %q", value, got, want)
		}
	}
}
