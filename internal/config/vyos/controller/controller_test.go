package controller

import (
	"bytes"
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	configv1alpha1 "github.com/hauke-cloud/router-api/api/config/v1alpha1"
	corev1alpha1 "github.com/hauke-cloud/router-api/api/core/v1alpha1"
	"github.com/hauke-cloud/router-api/internal/conditions"
	"github.com/hauke-cloud/router-api/internal/config/vyos/vyostest"
	"github.com/hauke-cloud/router-api/internal/testenv"
)

var k8s client.Client

func TestMain(m *testing.M) {
	os.Exit(testenv.Run(m, func(c client.Client) { k8s = c }))
}

const userCommands = "set system time-zone {{ .Values.zone }}\nset system host-name {{ .Router.Name }}\n"

type fixture struct {
	t         *testing.T
	ctx       context.Context
	namespace string
	now       time.Time
	router    *vyostest.Router
	r         *Reconciler
}

// newFixture creates a Router named edge that owns a VyOSConfig named edge,
// and a fake VyOS router. The Router has no address yet.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{
		t: t, ctx: context.Background(), namespace: testenv.Namespace(t, k8s), now: time.Now(),
		router: vyostest.New(t, "set service ntp server time1.vyos.net\n"),
	}
	f.r = &Reconciler{Client: k8s, Now: func() time.Time { return f.now }}

	owner := &corev1alpha1.Router{
		ObjectMeta: metav1.ObjectMeta{
			Name: "edge", Namespace: f.namespace,
			Labels: map[string]string{corev1alpha1.DeploymentNameLabel: "edge"},
		},
		Spec: corev1alpha1.RouterSpec{
			MachineRef: corev1alpha1.LocalObjectReference{Name: "edge"},
			ConfigRef:  corev1alpha1.ContractReference{APIGroup: configv1alpha1.GroupVersion.Group, Kind: "VyOSConfig", Name: "edge"},
		},
	}
	if err := k8s.Create(f.ctx, owner); err != nil {
		t.Fatal(err)
	}
	config := &configv1alpha1.VyOSConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: "edge", Namespace: f.namespace,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(owner, corev1alpha1.GroupVersion.WithKind("Router"))},
		},
		Spec: configv1alpha1.VyOSConfigSpec{
			Image:    "ghcr.io/hauke-cloud/vyos@sha256:abc",
			Commands: userCommands,
			Values:   []configv1alpha1.Value{{Name: "zone", ValueSource: configv1alpha1.ValueSource{Value: ptr.To("UTC")}}},
			Management: configv1alpha1.ManagementSpec{
				Port: f.router.Port(),
			},
		},
	}
	if err := k8s.Create(f.ctx, config); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) key(name string) types.NamespacedName {
	return types.NamespacedName{Namespace: f.namespace, Name: name}
}

func (f *fixture) reconcile() reconcile.Result {
	f.t.Helper()
	result, err := f.r.Reconcile(f.ctx, reconcile.Request{NamespacedName: f.key("edge")})
	if err != nil {
		f.t.Fatalf("Reconcile: %v", err)
	}
	return result
}

func (f *fixture) config() *configv1alpha1.VyOSConfig {
	f.t.Helper()
	config := &configv1alpha1.VyOSConfig{}
	if err := k8s.Get(f.ctx, f.key("edge"), config); err != nil {
		f.t.Fatal(err)
	}
	return config
}

func (f *fixture) updateConfig(mutate func(*configv1alpha1.VyOSConfig)) {
	f.t.Helper()
	config := f.config()
	mutate(config)
	if err := k8s.Update(f.ctx, config); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) secret(name string) *corev1.Secret {
	f.t.Helper()
	secret := &corev1.Secret{}
	if err := k8s.Get(f.ctx, f.key(name), secret); err != nil {
		f.t.Fatalf("get Secret %s: %v", name, err)
	}
	return secret
}

// boot plays everything between the bootstrap data being published and the
// router answering: the server exists, VyOS runs, the seed has given it the
// credentials. In the test the fake router's own certificate and key stand in
// for the generated ones.
func (f *fixture) boot() {
	f.t.Helper()
	f.reconcile()
	credentials := f.secret("edge-credentials")
	credentials.Data[CredentialsAPIKey] = []byte(f.router.Key)
	credentials.Data[corev1.TLSCertKey] = f.router.CertPEM
	credentials.Data[corev1.TLSPrivateKeyKey] = f.router.KeyPEM
	credentials.Data[CredentialsServerName] = []byte(vyostest.ServerName)
	if err := k8s.Update(f.ctx, credentials); err != nil {
		f.t.Fatal(err)
	}
	f.setAddress("edge", "127.0.0.1", "10.0.1.2")
}

func (f *fixture) setAddress(router, external, internal string) {
	f.t.Helper()
	owner := &corev1alpha1.Router{}
	if err := k8s.Get(f.ctx, f.key(router), owner); err != nil {
		f.t.Fatal(err)
	}
	owner.Status.Addresses = []corev1alpha1.MachineAddress{
		{Type: corev1alpha1.AddressExternalIP, Address: external},
		{Type: corev1alpha1.AddressInternalIP, Address: internal},
	}
	if err := k8s.Status().Update(f.ctx, owner); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) condition(conditionType string) string {
	f.t.Helper()
	condition := conditions.Get(f.config().Status.Conditions, conditionType)
	if condition == nil {
		return ""
	}
	return string(condition.Status) + "/" + condition.Reason
}

func (f *fixture) running(line string) bool {
	return slices.Contains(f.router.Running(), line)
}

func TestPublishesBootstrapData(t *testing.T) {
	f := newFixture(t)

	f.reconcile()

	config := f.config()
	if config.Status.DataSecretName != "edge-bootstrap" ||
		config.Status.Initialization == nil || !ptr.Deref(config.Status.Initialization.DataSecretCreated, false) {
		t.Fatalf("status = %+v", config.Status)
	}
	userData := string(f.secret("edge-bootstrap").Data["value"])
	if !strings.HasPrefix(userData, "#cloud-config\n") {
		t.Errorf("user data = %.40q", userData)
	}
	credentials := f.secret("edge-credentials")
	for _, key := range []string{CredentialsAPIKey, corev1.TLSCertKey, corev1.TLSPrivateKeyKey, CredentialsServerName} {
		if len(credentials.Data[key]) == 0 {
			t.Errorf("credentials have no %s", key)
		}
	}
	for _, secret := range []*corev1.Secret{credentials, f.secret("edge-bootstrap")} {
		if !metav1.IsControlledBy(secret, config) {
			t.Errorf("Secret %s is not owned by the config: it would outlive the router", secret.Name)
		}
	}
	if got := f.condition(corev1alpha1.ReadyCondition); !strings.HasPrefix(got, "False/") {
		t.Errorf("Ready = %s before the router exists", got)
	}

	// The server is created from this data exactly once. Whatever changes
	// later, the data must not: it has to keep describing the server that
	// exists.
	f.updateConfig(func(config *configv1alpha1.VyOSConfig) { config.Spec.Image = "ghcr.io/hauke-cloud/vyos@sha256:def" })
	f.reconcile()
	if again := string(f.secret("edge-bootstrap").Data["value"]); again != userData {
		t.Error("the bootstrap data changed after it was published")
	}
	if !bytes.Equal(f.secret("edge-credentials").Data[CredentialsAPIKey], credentials.Data[CredentialsAPIKey]) {
		t.Error("the API key was regenerated")
	}
}

func TestWaitsForAnAddress(t *testing.T) {
	f := newFixture(t)

	result := f.reconcile()

	if got := f.condition(configv1alpha1.APIReachableCondition); got != "False/"+ReasonWaitingForAddress {
		t.Errorf("APIReachable = %s", got)
	}
	if result.RequeueAfter == 0 {
		t.Error("no requeue")
	}
	if n := len(f.router.Requests()); n != 0 {
		t.Errorf("%d requests were sent somewhere", n)
	}
}

func TestAppliesTheConfiguration(t *testing.T) {
	f := newFixture(t)
	f.boot()

	f.reconcile()

	for _, want := range []string{
		"set system time-zone UTC",
		"set system host-name edge",
		"set service https api rest",
		"set system config-management commit-revisions 100",
	} {
		if !f.running(want) {
			t.Errorf("the router does not run %q; it runs %q", want, f.router.Running())
		}
	}
	// Not listed by anyone, so it goes: the configuration is the whole
	// truth, not a patch on top of whatever was there.
	if f.running("set service ntp server time1.vyos.net") {
		t.Error("configuration nobody asked for is still there")
	}
	if !slices.Equal(f.router.Running(), f.router.Saved()) {
		t.Error("the configuration was applied but not saved")
	}

	for conditionType, want := range map[string]string{
		corev1alpha1.ConfigAppliedCondition:  "True/" + ReasonApplied,
		corev1alpha1.HealthyCondition:        "True/" + ReasonHealthy,
		configv1alpha1.APIReachableCondition: "True/" + ReasonReachable,
		corev1alpha1.ReadyCondition:          "True/" + ReasonReady,
	} {
		if got := f.condition(conditionType); got != want {
			t.Errorf("%s = %s, want %s", conditionType, got, want)
		}
	}
	status := f.config().Status
	if status.Version != "2026.10.07-0712-rolling" || status.AppliedHash == "" || status.RunningHash == "" || status.LastAppliedTime == nil {
		t.Errorf("status = %+v", status)
	}

	// Nothing changed, so nothing is sent.
	configures := f.router.Configures()
	f.reconcile()
	if f.router.Configures() != configures {
		t.Error("a second reconcile changed the router again")
	}
}

func TestValuesFromSecrets(t *testing.T) {
	f := newFixture(t)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "wg", Namespace: f.namespace},
		Data:       map[string][]byte{"key": []byte("kKMnp6QBm1Wj0zxnc1uBbSs3hhdpVIFeQy9hxRTBxWQ=")},
	}
	if err := k8s.Create(f.ctx, secret); err != nil {
		t.Fatal(err)
	}
	f.updateConfig(func(config *configv1alpha1.VyOSConfig) {
		config.Spec.Commands = "set interfaces wireguard wg0 private-key {{ .Values.wgKey }}\n"
		config.Spec.Values = []configv1alpha1.Value{{Name: "wgKey", ValueSource: configv1alpha1.ValueSource{
			SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "wg"}, Key: "key"},
		}}}
	})
	f.boot()

	f.reconcile()

	if !f.running("set interfaces wireguard wg0 private-key kKMnp6QBm1Wj0zxnc1uBbSs3hhdpVIFeQy9hxRTBxWQ=") {
		t.Errorf("running = %q", f.router.Running())
	}
}

func TestARenderErrorLeavesTheRouterAlone(t *testing.T) {
	f := newFixture(t)
	f.boot()
	f.reconcile()
	before := f.router.Running()

	f.updateConfig(func(config *configv1alpha1.VyOSConfig) {
		config.Spec.Commands = "set system host-name {{ .Values.typo }}\n"
	})
	f.reconcile()

	if got := f.condition(corev1alpha1.ConfigAppliedCondition); got != "False/"+ReasonRenderFailed {
		t.Errorf("ConfigApplied = %s", got)
	}
	if !slices.Equal(f.router.Running(), before) {
		t.Error("a configuration that could not be rendered still changed the router")
	}
	// The router itself is fine.
	if got := f.condition(corev1alpha1.HealthyCondition); got != "True/"+ReasonHealthy {
		t.Errorf("Healthy = %s", got)
	}
}

func TestARejectedConfigurationIsNotHammered(t *testing.T) {
	f := newFixture(t)
	f.boot()
	f.reconcile()
	before := f.router.Running()

	f.router.RejectCommitsOf("firewall")
	f.updateConfig(func(config *configv1alpha1.VyOSConfig) {
		config.Spec.Commands = userCommands + "set firewall ipv4 input filter default-action drop\n"
	})
	f.reconcile()

	if got := f.condition(corev1alpha1.ConfigAppliedCondition); got != "False/"+ReasonCommitFailed {
		t.Fatalf("ConfigApplied = %s", got)
	}
	message := conditions.Get(f.config().Status.Conditions, corev1alpha1.ConfigAppliedCondition).Message
	if !strings.Contains(message, "firewall") {
		t.Errorf("message = %q, want the router's explanation", message)
	}
	if !slices.Equal(f.router.Running(), before) {
		t.Errorf("the router was left half configured: %q", f.router.Running())
	}

	// The same configuration is not tried again right away: each attempt is
	// a commit and a rollback on a production router.
	configures := f.router.Configures()
	f.reconcile()
	f.reconcile()
	if f.router.Configures() != configures {
		t.Error("the rejected configuration was applied again immediately")
	}

	// But it is after a while, in case the cause was on the router.
	f.router.AllowCommits()
	f.now = f.now.Add(RetryInterval + time.Second)
	f.reconcile()
	if got := f.condition(corev1alpha1.ConfigAppliedCondition); got != "True/"+ReasonApplied {
		t.Errorf("ConfigApplied = %s after the retry interval", got)
	}
}

func TestAFailedConfirmIsNotReportedAsApplied(t *testing.T) {
	f := newFixture(t)
	f.boot()
	f.reconcile()

	// The change commits, and then the confirmation does not get through.
	f.router.FailNext("/config-file", 1)
	f.updateConfig(func(config *configv1alpha1.VyOSConfig) {
		config.Spec.Commands = userCommands + "set system name-server 1.1.1.1\n"
	})
	f.reconcile()
	if got := f.condition(corev1alpha1.ConfigAppliedCondition); got != "False/"+ReasonUnconfirmed {
		t.Fatalf("ConfigApplied = %s", got)
	}
	if !f.router.ConfirmPending() {
		t.Fatal("test setup: no commit-confirm is pending")
	}

	// The router still runs the new configuration, so there is nothing to
	// change. But its revert timer is running and nothing was saved: saying
	// "applied" now would be saying it about a configuration the router is
	// about to undo.
	f.router.FailNext("/config-file", 1)
	f.reconcile()
	if got := f.condition(corev1alpha1.ConfigAppliedCondition); got == "True/"+ReasonApplied {
		t.Fatal("ConfigApplied is True while the commit is unconfirmed and the confirmation still fails")
	}

	// The connection is back. The pending commit is confirmed and saved,
	// instead of being left to revert and applied all over again.
	configures := f.router.Configures()
	f.reconcile()
	if got := f.condition(corev1alpha1.ConfigAppliedCondition); got != "True/"+ReasonApplied {
		t.Errorf("ConfigApplied = %s", got)
	}
	if f.router.ConfirmPending() {
		t.Error("the revert timer is still running")
	}
	if !slices.Equal(f.router.Running(), f.router.Saved()) {
		t.Error("the configuration is reported as applied and was never saved")
	}
	if f.router.Configures() != configures {
		t.Error("the configuration was committed again instead of the pending commit being confirmed")
	}
}

func TestAChangedConfigurationIsTriedAtOnce(t *testing.T) {
	f := newFixture(t)
	f.boot()
	f.reconcile()
	f.router.RejectCommitsOf("firewall")
	f.updateConfig(func(config *configv1alpha1.VyOSConfig) {
		config.Spec.Commands = userCommands + "set firewall ipv4 input filter default-action drop\n"
	})
	f.reconcile()

	// The user fixes it. That must not have to wait out the retry interval.
	f.updateConfig(func(config *configv1alpha1.VyOSConfig) {
		config.Spec.Commands = userCommands + "set system name-server 1.1.1.1\n"
	})
	f.reconcile()

	if !f.running("set system name-server 1.1.1.1") {
		t.Errorf("ConfigApplied = %s; running = %q", f.condition(corev1alpha1.ConfigAppliedCondition), f.router.Running())
	}
}

func TestSecretsDoNotLeakIntoStatus(t *testing.T) {
	f := newFixture(t)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: f.namespace},
		Data:       map[string][]byte{"v": []byte("hunter2hunter2")},
	}
	if err := k8s.Create(f.ctx, secret); err != nil {
		t.Fatal(err)
	}
	f.boot()
	f.reconcile()
	// A router quotes the offending part of a commit in its error.
	f.router.RejectCommitsOf("firewall", "hunter2hunter2")
	f.updateConfig(func(config *configv1alpha1.VyOSConfig) {
		config.Spec.Commands = userCommands + "set firewall {{ .Values.secret }} x\n"
		config.Spec.Values = append(config.Spec.Values, configv1alpha1.Value{Name: "secret", ValueSource: configv1alpha1.ValueSource{
			SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "s"}, Key: "v"},
		}})
	})

	f.reconcile()

	for _, condition := range f.config().Status.Conditions {
		if strings.Contains(condition.Message, "hunter2hunter2") {
			t.Errorf("%s quotes a value from a Secret: %q", condition.Type, condition.Message)
		}
	}
	if got := f.condition(corev1alpha1.ConfigAppliedCondition); got != "False/"+ReasonCommitFailed {
		t.Errorf("ConfigApplied = %s", got)
	}
}

func TestDriftIsCorrected(t *testing.T) {
	f := newFixture(t)
	f.boot()
	f.reconcile()
	want := f.router.Running()

	// Someone logs in and changes something by hand.
	f.router.Apply(t, strings.Join(want, "\n")+"\nset system name-server 8.8.8.8\n")
	f.reconcile()

	if !slices.Equal(f.router.Running(), want) {
		t.Errorf("running = %q, want the manual change gone", f.router.Running())
	}
}

func TestPeersAreRendered(t *testing.T) {
	f := newFixture(t)
	peer := &corev1alpha1.Router{
		ObjectMeta: metav1.ObjectMeta{
			Name: "edge-peer", Namespace: f.namespace,
			Labels: map[string]string{corev1alpha1.DeploymentNameLabel: "edge"},
		},
		Spec: corev1alpha1.RouterSpec{
			MachineRef: corev1alpha1.LocalObjectReference{Name: "edge-peer"},
			ConfigRef:  corev1alpha1.ContractReference{APIGroup: configv1alpha1.GroupVersion.Group, Kind: "VyOSConfig", Name: "edge-peer"},
		},
	}
	stranger := peer.DeepCopy()
	stranger.Name, stranger.Labels = "other", map[string]string{corev1alpha1.DeploymentNameLabel: "other"}
	for _, router := range []*corev1alpha1.Router{peer, stranger} {
		if err := k8s.Create(f.ctx, router); err != nil {
			t.Fatal(err)
		}
	}
	f.setAddress("edge-peer", "203.0.113.8", "10.0.1.3")
	f.setAddress("other", "203.0.113.9", "10.0.1.4")
	f.updateConfig(func(config *configv1alpha1.VyOSConfig) {
		config.Spec.Commands = `set high-availability vrrp group wan hello-source-address {{ .Machine.InternalIP }}
{{- range .Peers }}
set high-availability vrrp group wan peer-address {{ .InternalIP }}
{{- end }}
`
	})
	f.boot()

	f.reconcile()

	if !f.running("set high-availability vrrp group wan hello-source-address 10.0.1.2") ||
		!f.running("set high-availability vrrp group wan peer-address 10.0.1.3") {
		t.Errorf("running = %q", f.router.Running())
	}
	if f.running("set high-availability vrrp group wan peer-address 10.0.1.4") {
		t.Error("a router of another group was rendered as a peer")
	}
}

func TestVRRPStateAndFault(t *testing.T) {
	f := newFixture(t)
	f.boot()
	f.router.SetVRRP("Name  Interface  VRID  State   Priority  Last Transition\n----  ----  ----  ----  ----  ----\nwan   eth1       10    MASTER  100       5m\n")
	f.reconcile()
	if got := f.condition(configv1alpha1.VRRPStateCondition); got != "True/MASTER" {
		t.Errorf("VRRPMaster = %s", got)
	}
	// What core goes by when it decides which router to touch first.
	if got := f.condition(corev1alpha1.ActiveCondition); got != "True/MASTER" {
		t.Errorf("Active = %s", got)
	}

	f.router.SetVRRP("Name  Interface  VRID  State   Priority  Last Transition\n----  ----  ----  ----  ----  ----\nwan   eth1       10    FAULT  100       5s\n")
	f.reconcile()
	if got := f.condition(configv1alpha1.VRRPStateCondition); got != "False/FAULT" {
		t.Errorf("VRRPMaster = %s", got)
	}
	if got := f.condition(corev1alpha1.HealthyCondition); got != "False/"+ReasonVRRPFault {
		t.Errorf("Healthy = %s", got)
	}
	if got := f.condition(corev1alpha1.ReadyCondition); !strings.HasPrefix(got, "False/") {
		t.Errorf("Ready = %s", got)
	}
}

func TestDrain(t *testing.T) {
	const vrrpConfig = "set high-availability vrrp group wan vrid 10\n"
	f := newFixture(t)
	f.updateConfig(func(config *configv1alpha1.VyOSConfig) { config.Spec.Commands = vrrpConfig })
	f.boot()
	f.router.SetVRRP("Name  Interface  VRID  State   Priority  Last Transition\n----  ----  ----  ----  ----  ----\nwan   eth1       10    MASTER  100       5m\n")
	f.reconcile()

	f.updateConfig(func(config *configv1alpha1.VyOSConfig) {
		config.Annotations = map[string]string{corev1alpha1.DrainAnnotation: "now"}
	})
	f.reconcile()

	if !f.running("set high-availability disable") {
		t.Fatalf("VRRP was not switched off; running = %q", f.router.Running())
	}
	// Still reported as master by the router: not drained yet.
	if got := f.condition(corev1alpha1.DrainedCondition); !strings.HasPrefix(got, "False/") {
		t.Errorf("Drained = %s while the router still reports MASTER", got)
	}

	f.router.SetVRRP("VRRP data is not available (process not running or no active groups)\n")
	f.reconcile()
	if got := f.condition(corev1alpha1.DrainedCondition); got != "True/"+ReasonDrained {
		t.Errorf("Drained = %s", got)
	}

	// And back.
	f.updateConfig(func(config *configv1alpha1.VyOSConfig) { config.Annotations = nil })
	f.reconcile()
	if f.running("set high-availability disable") {
		t.Error("VRRP stayed switched off after the drain request was withdrawn")
	}
}

func TestUnreachableRouter(t *testing.T) {
	f := newFixture(t)
	f.boot()
	f.reconcile()
	// The router goes away.
	f.setAddress("edge", "127.0.0.1", "10.0.1.2")
	f.updateConfig(func(config *configv1alpha1.VyOSConfig) { config.Spec.Management.Port = 1 })

	result := f.reconcile()

	if got := f.condition(configv1alpha1.APIReachableCondition); got != "False/"+ReasonUnreachable {
		t.Errorf("APIReachable = %s", got)
	}
	if got := f.condition(corev1alpha1.HealthyCondition); got != "False/"+ReasonUnreachable {
		t.Errorf("Healthy = %s", got)
	}
	if result.RequeueAfter == 0 {
		t.Error("no requeue")
	}
}

func TestTemplateReplacementHash(t *testing.T) {
	ctx := context.Background()
	namespace := testenv.Namespace(t, k8s)
	template := &configv1alpha1.VyOSConfigTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: namespace},
		Spec: configv1alpha1.VyOSConfigTemplateSpec{Template: configv1alpha1.VyOSConfigTemplateResource{
			Spec: configv1alpha1.VyOSConfigSpec{Image: "ghcr.io/hauke-cloud/vyos:1", Commands: "set system time-zone UTC\n"},
		}},
	}
	if err := k8s.Create(ctx, template); err != nil {
		t.Fatal(err)
	}
	r := &TemplateReconciler{Client: k8s}
	key := types.NamespacedName{Namespace: namespace, Name: "edge"}
	hash := func() string {
		t.Helper()
		if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
			t.Fatal(err)
		}
		if err := k8s.Get(ctx, key, template); err != nil {
			t.Fatal(err)
		}
		if template.Status.ObservedGeneration != template.Generation {
			t.Errorf("observedGeneration = %d, generation = %d", template.Status.ObservedGeneration, template.Generation)
		}
		return template.Status.ReplacementHash
	}

	first := hash()
	if first == "" {
		t.Fatal("no replacement hash")
	}

	// A firewall rule is applied to the running routers.
	template.Spec.Template.Spec.Commands = "set system time-zone Europe/Berlin\n"
	template.Spec.Template.Spec.Management.ConfirmTimeoutMinutes = 5
	if err := k8s.Update(ctx, template); err != nil {
		t.Fatal(err)
	}
	if got := hash(); got != first {
		t.Error("the hash changed with the commands: every configuration change would replace the routers")
	}

	// A new image is not.
	template.Spec.Template.Spec.Image = "ghcr.io/hauke-cloud/vyos:2"
	if err := k8s.Update(ctx, template); err != nil {
		t.Fatal(err)
	}
	second := hash()
	if second == first {
		t.Error("the hash did not change with the image")
	}

	template.Spec.Template.Spec.Files = []configv1alpha1.File{{Path: "/config/x", ValueSource: configv1alpha1.ValueSource{Value: ptr.To("y")}}}
	if err := k8s.Update(ctx, template); err != nil {
		t.Fatal(err)
	}
	if got := hash(); got == second {
		t.Error("the hash did not change with the files")
	}
}

func TestTheAPIRejectsWhatMustNotReachARouter(t *testing.T) {
	namespace := testenv.Namespace(t, k8s)
	tests := map[string]func(*configv1alpha1.VyOSConfigSpec){
		// The image name is written into a systemd unit file on the host. A
		// line break in it would be a line of the unit file.
		"image with a line break": func(spec *configv1alpha1.VyOSConfigSpec) {
			spec.Image = "ghcr.io/hauke-cloud/vyos:1\nPodmanArgs=--volume=/:/host"
		},
		"image with a space": func(spec *configv1alpha1.VyOSConfigSpec) { spec.Image = "ghcr.io/hauke-cloud/vyos:1 --privileged" },
		"allowed source that is no address": func(spec *configv1alpha1.VyOSConfigSpec) {
			spec.Management.AllowedSources = []string{"the-lab"}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			config := &configv1alpha1.VyOSConfig{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "rejected-", Namespace: namespace},
				Spec:       configv1alpha1.VyOSConfigSpec{Image: "ghcr.io/hauke-cloud/vyos:1"},
			}
			mutate(&config.Spec)
			if err := k8s.Create(context.Background(), config); err == nil {
				t.Error("the API server accepted it")
			}
		})
	}

	for name, spec := range map[string]configv1alpha1.VyOSConfigSpec{
		"tag":    {Image: "ghcr.io/hauke-cloud/vyos:2026.10.07-0712-rolling"},
		"digest": {Image: "ghcr.io/hauke-cloud/vyos@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
		"port":   {Image: "registry.lab.example:5000/vyos:dev"},
		"sources": {Image: "localhost/vyos:dev", Management: configv1alpha1.ManagementSpec{
			AllowedSources: []string{"192.0.2.0/24", "198.51.100.7", "2001:db8::/32"},
		}},
	} {
		t.Run("accepts "+name, func(t *testing.T) {
			config := &configv1alpha1.VyOSConfig{ObjectMeta: metav1.ObjectMeta{GenerateName: "accepted-", Namespace: namespace}, Spec: spec}
			if err := k8s.Create(context.Background(), config); err != nil {
				t.Errorf("rejected: %v", err)
			}
		})
	}
}

// inSlot puts the fixture's router into a slot.
func (f *fixture) inSlot(router, slot string) {
	f.t.Helper()
	owner := &corev1alpha1.Router{}
	if err := k8s.Get(f.ctx, f.key(router), owner); err != nil {
		f.t.Fatal(err)
	}
	owner.Labels[corev1alpha1.SlotLabel] = slot
	if err := k8s.Update(f.ctx, owner); err != nil {
		f.t.Fatal(err)
	}
}

func TestEverySlotHasItsOwnValues(t *testing.T) {
	f := newFixture(t)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "wg-1", Namespace: f.namespace},
		Data:       map[string][]byte{"key": []byte("xo2pQF7WbTFQkAOgNi6s9RNdu1uEhn2wGYOvUkb7HBg=")},
	}
	if err := k8s.Create(f.ctx, secret); err != nil {
		t.Fatal(err)
	}
	f.inSlot("edge", "1")
	f.updateConfig(func(config *configv1alpha1.VyOSConfig) {
		config.Spec.Commands = `set system time-zone {{ .Values.zone }}
set interfaces wireguard wg0 address {{ .Values.tunnel }}
set interfaces wireguard wg0 private-key {{ .Values.key }}
set interfaces wireguard wg0 description 'slot {{ .Router.Slot }}'
`
		config.Spec.Values = []configv1alpha1.Value{
			{Name: "zone", ValueSource: configv1alpha1.ValueSource{Value: ptr.To("UTC")}},
			{Name: "tunnel", ValueSource: configv1alpha1.ValueSource{Value: ptr.To("overridden in every slot")}},
		}
		config.Spec.Slots = []configv1alpha1.SlotSpec{
			{Values: []configv1alpha1.Value{
				{Name: "tunnel", ValueSource: configv1alpha1.ValueSource{Value: ptr.To("10.99.0.1/30")}},
				{Name: "key", ValueSource: configv1alpha1.ValueSource{Value: ptr.To("key-of-slot-0")}},
			}},
			{Values: []configv1alpha1.Value{
				{Name: "tunnel", ValueSource: configv1alpha1.ValueSource{Value: ptr.To("10.99.0.5/30")}},
				{Name: "key", ValueSource: configv1alpha1.ValueSource{SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "wg-1"}, Key: "key",
				}}},
			}},
		}
	})
	f.boot()

	f.reconcile()

	for _, want := range []string{
		"set system time-zone UTC",
		"set interfaces wireguard wg0 address 10.99.0.5/30",
		"set interfaces wireguard wg0 private-key xo2pQF7WbTFQkAOgNi6s9RNdu1uEhn2wGYOvUkb7HBg=",
		"set interfaces wireguard wg0 description 'slot 1'",
	} {
		if !f.running(want) {
			t.Errorf("the router does not run %q; ConfigApplied = %s; running = %q",
				want, f.condition(corev1alpha1.ConfigAppliedCondition), f.router.Running())
		}
	}
}

func TestASlotWithoutValuesIsAnError(t *testing.T) {
	f := newFixture(t)
	f.inSlot("edge", "2")
	f.updateConfig(func(config *configv1alpha1.VyOSConfig) {
		config.Spec.Slots = []configv1alpha1.SlotSpec{{}, {}}
	})
	f.boot()
	before := f.router.Running()

	f.reconcile()

	// A third router configured with nobody's values, or the first slot's,
	// would come up as a second copy of another router.
	if got := f.condition(corev1alpha1.ConfigAppliedCondition); got != "False/"+ReasonRenderFailed {
		t.Errorf("ConfigApplied = %s", got)
	}
	if !slices.Equal(f.router.Running(), before) {
		t.Error("the router was configured anyway")
	}
}

func TestPeersCarryTheirSlot(t *testing.T) {
	f := newFixture(t)
	peer := &corev1alpha1.Router{
		ObjectMeta: metav1.ObjectMeta{
			Name: "edge-peer", Namespace: f.namespace,
			Labels: map[string]string{corev1alpha1.DeploymentNameLabel: "edge", corev1alpha1.SlotLabel: "0"},
		},
		Spec: corev1alpha1.RouterSpec{
			MachineRef: corev1alpha1.LocalObjectReference{Name: "edge-peer"},
			ConfigRef:  corev1alpha1.ContractReference{APIGroup: configv1alpha1.GroupVersion.Group, Kind: "VyOSConfig", Name: "edge-peer"},
		},
	}
	if err := k8s.Create(f.ctx, peer); err != nil {
		t.Fatal(err)
	}
	f.setAddress("edge-peer", "203.0.113.8", "10.0.1.3")
	f.inSlot("edge", "1")
	f.updateConfig(func(config *configv1alpha1.VyOSConfig) {
		config.Spec.Commands = "{{ range .Peers }}set system static-host-mapping host-name slot-{{ .Slot }} inet {{ .InternalIP }}\n{{ end }}"
	})
	f.boot()

	f.reconcile()

	if !f.running("set system static-host-mapping host-name slot-0 inet 10.0.1.3") {
		t.Errorf("running = %q", f.router.Running())
	}
}
