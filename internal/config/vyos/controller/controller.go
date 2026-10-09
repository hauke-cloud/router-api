// Package controller holds the controllers of the VyOS config provider.
package controller

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	configv1alpha1 "github.com/hauke-cloud/router-api/api/config/v1alpha1"
	corev1alpha1 "github.com/hauke-cloud/router-api/api/core/v1alpha1"
	"github.com/hauke-cloud/router-api/internal/conditions"
	"github.com/hauke-cloud/router-api/internal/config/vyos/apply"
	"github.com/hauke-cloud/router-api/internal/config/vyos/bootstrap"
	vyosclient "github.com/hauke-cloud/router-api/internal/config/vyos/client"
	"github.com/hauke-cloud/router-api/internal/config/vyos/command"
	"github.com/hauke-cloud/router-api/internal/config/vyos/render"
	"github.com/hauke-cloud/router-api/internal/config/vyos/vrrp"
	"github.com/hauke-cloud/router-api/internal/contract"
	"github.com/hauke-cloud/router-api/internal/pace"
)

// Condition reasons set by this provider.
const (
	ReasonReady             = "Ready"
	ReasonWaitingForOwner   = "WaitingForOwner"
	ReasonWaitingForAddress = "WaitingForAddress"
	ReasonBootstrapFailed   = "BootstrapFailed"
	ReasonBootstrapPending  = "BootstrapPending"
	ReasonReachable         = "Reachable"
	ReasonUnreachable       = "Unreachable"
	ReasonApplied           = "Applied"
	ReasonRenderFailed      = "RenderFailed"
	ReasonCommitFailed      = "CommitFailed"
	ReasonUnconfirmed       = "Unconfirmed"
	ReasonApplyFailed       = "ApplyFailed"
	ReasonNotApplied        = "NotApplied"
	ReasonHealthy           = "Healthy"
	ReasonVRRPFault         = "VRRPFault"
	ReasonDrained           = "Drained"
	ReasonDraining          = "Draining"
	ReasonNotDraining       = "NotDraining"
	ReasonPaused            = "Paused"
)

// Keys of the credentials Secret, next to the usual tls.crt and tls.key.
const (
	CredentialsAPIKey     = "apiKey"
	CredentialsServerName = "serverName"
)

const (
	// RetryInterval is how long a configuration the router rejected is left
	// alone before it is tried again. Every attempt is a commit and a
	// rollback on a router that is carrying traffic.
	RetryInterval = 10 * time.Minute
	// checkInterval is how often a router is looked at.
	checkInterval = 30 * time.Second
	// waitInterval is how soon to look again while waiting for the router
	// to appear.
	waitInterval = 10 * time.Second
	// requestTimeout bounds one reconcile's conversation with a router that
	// answers slowly or not at all.
	requestTimeout = 3 * time.Minute
	// maxMessageLength keeps a router's error from filling the status.
	maxMessageLength = 1024
)

// Reconciler reconciles VyOSConfigs.
type Reconciler struct {
	client.Client
	// Now is the clock. Defaults to time.Now.
	Now func() time.Time
}

// SetupWithManager registers the controller. The Secrets it creates are not
// watched: that would mean caching every Secret the manager can see. A
// router is looked at every checkInterval anyway.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&configv1alpha1.VyOSConfig{}).
		Complete(r)
}

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Reconcile brings one VyOSConfig, and the router it describes, up to date.
func (r *Reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	config := &configv1alpha1.VyOSConfig{}
	if err := r.Get(ctx, req.NamespacedName, config); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if !config.DeletionTimestamp.IsZero() {
		// The Secrets are owned by the config and go with it. The router
		// itself goes with its server.
		return reconcile.Result{}, nil
	}

	original := config.DeepCopy()
	result, err := r.reconcile(ctx, config)
	summarize(config)
	if !equality.Semantic.DeepEqual(config.Status, original.Status) {
		if statusErr := client.IgnoreNotFound(r.Status().Patch(ctx, config, client.MergeFrom(original))); statusErr != nil && err == nil {
			err = statusErr
		}
	}
	return result, err
}

// summarize derives Ready from the other conditions.
func summarize(config *configv1alpha1.VyOSConfig) {
	status := &config.Status
	generation := config.Generation
	bootstrapped := status.Initialization != nil && ptr.Deref(status.Initialization.DataSecretCreated, false)
	switch {
	case !bootstrapped:
		conditions.False(&status.Conditions, generation, corev1alpha1.ReadyCondition, ReasonBootstrapPending, "the bootstrap data has not been published")
	case !conditions.IsTrue(status.Conditions, corev1alpha1.ConfigAppliedCondition):
		conditions.False(&status.Conditions, generation, corev1alpha1.ReadyCondition, ReasonNotApplied, explain(status.Conditions, corev1alpha1.ConfigAppliedCondition))
	case !conditions.IsTrue(status.Conditions, corev1alpha1.HealthyCondition):
		conditions.False(&status.Conditions, generation, corev1alpha1.ReadyCondition, ReasonUnreachable, explain(status.Conditions, corev1alpha1.HealthyCondition))
	default:
		conditions.True(&status.Conditions, generation, corev1alpha1.ReadyCondition, ReasonReady, "")
	}
}

func explain(all []metav1.Condition, conditionType string) string {
	condition := conditions.Get(all, conditionType)
	if condition == nil {
		return conditionType + " has not been determined"
	}
	if condition.Message != "" {
		return condition.Message
	}
	return conditionType + " is " + string(condition.Status) + ": " + condition.Reason
}

func (r *Reconciler) reconcile(ctx context.Context, config *configv1alpha1.VyOSConfig) (reconcile.Result, error) {
	status := &config.Status
	generation := config.Generation
	status.ObservedGeneration = generation

	owner, err := r.owner(ctx, config)
	if err != nil {
		return reconcile.Result{}, err
	}
	if owner == nil {
		conditions.False(&status.Conditions, generation, corev1alpha1.ConfigAppliedCondition, ReasonWaitingForOwner, "not owned by a Router yet")
		return reconcile.Result{RequeueAfter: pace.Every(waitInterval)}, nil
	}
	for _, object := range []client.Object{config, owner} {
		if _, paused := object.GetAnnotations()[corev1alpha1.PausedAnnotation]; paused {
			return reconcile.Result{}, nil
		}
	}

	credentials, err := r.ensureCredentials(ctx, config)
	if err != nil {
		return reconcile.Result{}, err
	}
	values, secrets, valuesErr := r.values(ctx, config, owner)

	if err := r.ensureBootstrap(ctx, config, owner, credentials, values, valuesErr); err != nil {
		return reconcile.Result{}, err
	}

	// From here on it is about the running router.
	address := managementAddress(owner, config)
	if address == "" {
		for _, conditionType := range []string{configv1alpha1.APIReachableCondition, corev1alpha1.HealthyCondition} {
			conditions.False(&status.Conditions, generation, conditionType, ReasonWaitingForAddress,
				fmt.Sprintf("the router has no %s address yet", addressType(config)))
		}
		keepApplied(config, ReasonWaitingForAddress, "the router does not exist yet")
		return reconcile.Result{RequeueAfter: pace.Every(waitInterval)}, nil
	}

	api, err := vyosclient.New(&vyosclient.Options{
		URL:        "https://" + net.JoinHostPort(address, strconv.Itoa(int(port(config)))),
		Key:        credentials.APIKey,
		CACertPEM:  credentials.CertPEM,
		ServerName: credentials.ServerName,
	})
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("credentials of %s are unusable: %w", config.Name, err)
	}
	defer api.Close()
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	redact := redactor(append(secrets, credentials.APIKey))

	info, err := api.Info(ctx)
	if err != nil {
		message := redact(err.Error())
		conditions.False(&status.Conditions, generation, configv1alpha1.APIReachableCondition, ReasonUnreachable, message)
		conditions.False(&status.Conditions, generation, corev1alpha1.HealthyCondition, ReasonUnreachable, message)
		keepApplied(config, ReasonUnreachable, "the router cannot be reached")
		return reconcile.Result{RequeueAfter: pace.Every(waitInterval)}, nil
	}
	status.Version = info.Version
	conditions.True(&status.Conditions, generation, configv1alpha1.APIReachableCondition, ReasonReachable, "")

	r.applyConfig(ctx, config, owner, api, credentials, values, valuesErr, redact)
	r.observe(ctx, config, api, redact)
	return reconcile.Result{RequeueAfter: pace.Every(checkInterval)}, nil
}

// keepApplied leaves ConfigApplied as it is if it has been determined before,
// and otherwise says why it has not: a router that is briefly unreachable has
// not stopped running its configuration.
func keepApplied(config *configv1alpha1.VyOSConfig, reason, message string) {
	applied := conditions.Get(config.Status.Conditions, corev1alpha1.ConfigAppliedCondition)
	if applied != nil && applied.ObservedGeneration == config.Generation && applied.Status == metav1.ConditionTrue {
		return
	}
	conditions.False(&config.Status.Conditions, config.Generation, corev1alpha1.ConfigAppliedCondition, reason, message)
}

func (r *Reconciler) owner(ctx context.Context, config *configv1alpha1.VyOSConfig) (*corev1alpha1.Router, error) {
	ref := metav1.GetControllerOf(config)
	if ref == nil || ref.Kind != "Router" {
		return nil, nil
	}
	owner := &corev1alpha1.Router{}
	err := r.Get(ctx, types.NamespacedName{Namespace: config.Namespace, Name: ref.Name}, owner)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	return owner, err
}

func port(config *configv1alpha1.VyOSConfig) int32 {
	if config.Spec.Management.Port != 0 {
		return config.Spec.Management.Port
	}
	return 443
}

func addressType(config *configv1alpha1.VyOSConfig) corev1alpha1.AddressType {
	if config.Spec.Management.AddressType != "" {
		return config.Spec.Management.AddressType
	}
	return corev1alpha1.AddressExternalIP
}

// managementAddress picks the address the operator talks to: the first of the
// configured type, IPv4 before IPv6.
func managementAddress(owner *corev1alpha1.Router, config *configv1alpha1.VyOSConfig) string {
	wanted := addressType(config)
	fallback := ""
	for _, address := range owner.Status.Addresses {
		if address.Type != wanted {
			continue
		}
		if parsed, err := netip.ParseAddr(address.Address); err == nil && parsed.Is6() {
			if fallback == "" {
				fallback = address.Address
			}
			continue
		}
		return address.Address
	}
	return fallback
}

func credentialsName(config *configv1alpha1.VyOSConfig) string { return config.Name + "-credentials" }
func bootstrapName(config *configv1alpha1.VyOSConfig) string   { return config.Name + "-bootstrap" }

func ownedBy(config *configv1alpha1.VyOSConfig) []metav1.OwnerReference {
	return []metav1.OwnerReference{*metav1.NewControllerRef(config, configv1alpha1.GroupVersion.WithKind("VyOSConfig"))}
}

// ensureCredentials returns the router's credentials, generating them the
// first time. They are never regenerated: the router was seeded with them.
func (r *Reconciler) ensureCredentials(ctx context.Context, config *configv1alpha1.VyOSConfig) (*bootstrap.Credentials, error) {
	secret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Namespace: config.Namespace, Name: credentialsName(config)}, secret)
	if err == nil {
		return &bootstrap.Credentials{
			APIKey:     string(secret.Data[CredentialsAPIKey]),
			CertPEM:    secret.Data[corev1.TLSCertKey],
			KeyPEM:     secret.Data[corev1.TLSPrivateKeyKey],
			ServerName: string(secret.Data[CredentialsServerName]),
		}, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}

	credentials, err := bootstrap.NewCredentials(config.Name + "." + config.Namespace + ".router-api.internal")
	if err != nil {
		return nil, err
	}
	secret = &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: credentialsName(config), Namespace: config.Namespace,
			Labels: config.Labels, OwnerReferences: ownedBy(config),
		},
		Data: map[string][]byte{
			CredentialsAPIKey:       []byte(credentials.APIKey),
			corev1.TLSCertKey:       credentials.CertPEM,
			corev1.TLSPrivateKeyKey: credentials.KeyPEM,
			CredentialsServerName:   []byte(credentials.ServerName),
		},
	}
	if err := r.Create(ctx, secret); err != nil {
		return nil, fmt.Errorf("create credentials: %w", err)
	}
	return credentials, nil
}

// values resolves spec.values. secrets are the values that came from Secrets,
// for redaction.
func (r *Reconciler) values(ctx context.Context, config *configv1alpha1.VyOSConfig, owner *corev1alpha1.Router) (values map[string]string, secrets []string, err error) {
	// The shared values, then the ones of the router's slot over them.
	all := config.Spec.Values
	if len(config.Spec.Slots) > 0 {
		slot, ok := slotOf(owner)
		switch {
		case !ok:
			return nil, nil, errors.New("the configuration has per-slot values, but the router has no slot; it needs a RouterDeployment with the Slots strategy")
		case slot >= len(config.Spec.Slots):
			return nil, nil, fmt.Errorf("the configuration has values for %d slots and the router is in slot %d", len(config.Spec.Slots), slot)
		}
		all = append(slices.Clone(all), config.Spec.Slots[slot].Values...)
	}

	values = make(map[string]string, len(all))
	for i := range all {
		value := &all[i]
		resolved, secret, err := r.resolve(ctx, config.Namespace, &value.ValueSource)
		if err != nil {
			return nil, secrets, fmt.Errorf("value %q: %w", value.Name, err)
		}
		values[value.Name] = resolved
		if secret {
			secrets = append(secrets, resolved)
		}
	}
	return values, secrets, nil
}

// slotOf returns the slot of a router and whether it has one.
func slotOf(router *corev1alpha1.Router) (int, bool) {
	slot, err := strconv.Atoi(router.Labels[corev1alpha1.SlotLabel])
	if err != nil || slot < 0 {
		return 0, false
	}
	return slot, true
}

func (r *Reconciler) resolve(ctx context.Context, namespace string, source *configv1alpha1.ValueSource) (value string, secret bool, err error) {
	switch {
	case source.Value != nil:
		return *source.Value, false, nil
	case source.SecretKeyRef != nil:
		object := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: source.SecretKeyRef.Name}, object); err != nil {
			return "", true, fmt.Errorf("secret %s: %w", source.SecretKeyRef.Name, err)
		}
		raw, ok := object.Data[source.SecretKeyRef.Key]
		if !ok {
			return "", true, fmt.Errorf("secret %s has no key %q", source.SecretKeyRef.Name, source.SecretKeyRef.Key)
		}
		return string(raw), true, nil
	case source.ConfigMapKeyRef != nil:
		object := &corev1.ConfigMap{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: source.ConfigMapKeyRef.Name}, object); err != nil {
			return "", false, fmt.Errorf("configmap %s: %w", source.ConfigMapKeyRef.Name, err)
		}
		raw, ok := object.Data[source.ConfigMapKeyRef.Key]
		if !ok {
			return "", false, fmt.Errorf("configmap %s has no key %q", source.ConfigMapKeyRef.Name, source.ConfigMapKeyRef.Key)
		}
		return raw, false, nil
	}
	return "", false, errors.New("no source is set")
}

func (r *Reconciler) params(config *configv1alpha1.VyOSConfig, owner *corev1alpha1.Router, credentials *bootstrap.Credentials) *bootstrap.Params {
	return &bootstrap.Params{
		Credentials:      credentials,
		Image:            config.Spec.Image,
		HostName:         owner.Name,
		Port:             port(config),
		AllowedSources:   config.Spec.Management.AllowedSources,
		ConfirmAction:    "reload",
		PublicInterface:  defaultString(config.Spec.Host.PublicInterface, "eth0"),
		PrivateInterface: defaultString(config.Spec.Host.PrivateInterface, "eth1"),
	}
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func (r *Reconciler) data(ctx context.Context, config *configv1alpha1.VyOSConfig, owner *corev1alpha1.Router, values map[string]string) (*render.Data, error) {
	data := &render.Data{
		Values: values,
		Router: routerData(owner),
		Host: render.Host{
			PublicInterface:  defaultString(config.Spec.Host.PublicInterface, "eth0"),
			PrivateInterface: defaultString(config.Spec.Host.PrivateInterface, "eth1"),
		},
	}
	data.Machine.ExternalIP, data.Machine.ExternalIPv6, data.Machine.InternalIP = addressesOf(owner)

	if data.Router.Group == "" {
		return data, nil
	}
	peers := &corev1alpha1.RouterList{}
	if err := r.List(ctx, peers, client.InNamespace(owner.Namespace),
		client.MatchingLabels{corev1alpha1.DeploymentNameLabel: data.Router.Group}); err != nil {
		return nil, err
	}
	for i := range peers.Items {
		peer := &peers.Items[i]
		if peer.Name == owner.Name || !peer.DeletionTimestamp.IsZero() {
			continue
		}
		external, _, internal := addressesOf(peer)
		if external == "" && internal == "" {
			// Not built yet. It appears in the configuration once it has
			// an address, and this router is then reconfigured.
			continue
		}
		slot, _ := slotOf(peer)
		data.Peers = append(data.Peers, render.Peer{Name: peer.Name, Slot: slot, ExternalIP: external, InternalIP: internal})
	}
	render.SortPeers(data.Peers)
	return data, nil
}

func routerData(owner *corev1alpha1.Router) render.Router {
	slot, _ := slotOf(owner)
	return render.Router{
		Name: owner.Name, Namespace: owner.Namespace,
		Group: owner.Labels[corev1alpha1.DeploymentNameLabel], Slot: slot,
	}
}

func addressesOf(router *corev1alpha1.Router) (external, externalV6, internal string) {
	for _, address := range router.Status.Addresses {
		parsed, err := netip.ParseAddr(address.Address)
		if err != nil {
			continue
		}
		switch {
		case address.Type == corev1alpha1.AddressExternalIP && parsed.Is4() && external == "":
			external = address.Address
		case address.Type == corev1alpha1.AddressExternalIP && parsed.Is6() && externalV6 == "":
			externalV6 = address.Address
		case address.Type == corev1alpha1.AddressInternalIP && internal == "":
			internal = address.Address
		}
	}
	return external, externalV6, internal
}

// ensureBootstrap publishes the user data the server is created with. It is
// written once and then left alone: it has to keep describing the server that
// was created from it. A change that needs different user data is a
// different router.
func (r *Reconciler) ensureBootstrap(ctx context.Context, config *configv1alpha1.VyOSConfig, owner *corev1alpha1.Router,
	credentials *bootstrap.Credentials, values map[string]string, valuesErr error) error {
	status := &config.Status
	secret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Namespace: config.Namespace, Name: bootstrapName(config)}, secret)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if err == nil {
		status.DataSecretName = secret.Name
		status.Initialization = &configv1alpha1.VyOSConfigInitialization{DataSecretCreated: ptr.To(true)}
		return nil
	}

	fail := func(err error) error {
		conditions.False(&status.Conditions, config.Generation, corev1alpha1.ConfigAppliedCondition, ReasonBootstrapFailed, err.Error())
		return nil
	}
	if valuesErr != nil {
		return fail(valuesErr)
	}
	params := r.params(config, owner, credentials)
	// Files are rendered before the server exists: there are no addresses
	// and no peers to refer to yet.
	data := &render.Data{
		Values: values,
		Router: routerData(owner),
		Host:   render.Host{PublicInterface: params.PublicInterface, PrivateInterface: params.PrivateInterface},
	}
	for i := range config.Spec.Files {
		file := &config.Spec.Files[i]
		content, _, err := r.resolve(ctx, config.Namespace, &file.ValueSource)
		if err != nil {
			return fail(fmt.Errorf("file %s: %w", file.Path, err))
		}
		rendered, err := render.Text(file.Path, content, data)
		if err != nil {
			return fail(fmt.Errorf("file %s: %w", file.Path, err))
		}
		params.Files = append(params.Files, bootstrap.File{Path: file.Path, Permissions: file.Permissions, Content: rendered})
	}
	userData, err := bootstrap.UserData(params)
	if err != nil {
		return fail(err)
	}

	secret = &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: bootstrapName(config), Namespace: config.Namespace,
			Labels: config.Labels, OwnerReferences: ownedBy(config),
		},
		Data: map[string][]byte{contract.BootstrapDataKey: []byte(userData)},
	}
	if err := r.Create(ctx, secret); err != nil {
		return fmt.Errorf("create bootstrap data: %w", err)
	}
	status.DataSecretName = secret.Name
	status.Initialization = &configv1alpha1.VyOSConfigInitialization{DataSecretCreated: ptr.To(true)}
	return nil
}

// redactor returns a function that removes the given secrets from a message
// and bounds its length. A router quotes the configuration it rejects.
func redactor(secrets []string) func(string) string {
	pairs := make([]string, 0, 2*len(secrets))
	for _, secret := range secrets {
		// Very short values would shred ordinary words.
		if len(secret) >= 6 {
			pairs = append(pairs, secret, "<redacted>")
		}
	}
	replacer := strings.NewReplacer(pairs...)
	return func(message string) string {
		message = replacer.Replace(message)
		if len(message) > maxMessageLength {
			message = message[:maxMessageLength] + "..."
		}
		return message
	}
}

// applyConfig renders the configuration and makes the router run it. What
// happened is recorded in ConfigApplied; nothing here is an error the
// reconcile should be retried for sooner than the next look at the router.
func (r *Reconciler) applyConfig(ctx context.Context, config *configv1alpha1.VyOSConfig, owner *corev1alpha1.Router, api *vyosclient.Client,
	credentials *bootstrap.Credentials, values map[string]string, valuesErr error, redact func(string) string) {
	status := &config.Status
	generation := config.Generation
	notApplied := func(reason string, err error) {
		conditions.False(&status.Conditions, generation, corev1alpha1.ConfigAppliedCondition, reason, redact(err.Error()))
	}

	if valuesErr != nil {
		notApplied(ReasonRenderFailed, valuesErr)
		return
	}
	data, err := r.data(ctx, config, owner, values)
	if err != nil {
		notApplied(ReasonRenderFailed, err)
		return
	}
	user, err := render.Commands(config.Spec.Commands, data)
	if err != nil {
		notApplied(ReasonRenderFailed, err)
		return
	}
	desired := bootstrap.Desired(user, r.params(config, owner, credentials))
	if _, draining := config.Annotations[corev1alpha1.DrainAnnotation]; draining && usesVRRP(desired) {
		// A keepalived that stops announces priority 0, and a peer takes
		// over at once. Lowering the priority would not do it: a backup
		// that does not preempt stays a backup.
		desired = append(desired, command.Path{"high-availability", "disable"})
	}
	desiredHash := command.Hash(desired)

	changes, runningHash, err := apply.Plan(ctx, api, desired, bootstrap.Keep)
	if err != nil {
		notApplied(ReasonApplyFailed, err)
		return
	}
	if len(changes) == 0 {
		// The router runs what is wanted. That is only the same as
		// "applied" if this configuration is known to have been confirmed
		// and saved. If the last word on it was a failure, or it was never
		// recorded as applied, a commit may still be waiting for its
		// confirmation, about to be undone by the router.
		if status.AppliedHash != desiredHash || status.FailedHash != "" {
			if err := apply.Settle(ctx, api); err != nil {
				if errors.Is(err, apply.ErrUnconfirmed) {
					notApplied(ReasonUnconfirmed, err)
				} else {
					notApplied(ReasonApplyFailed, err)
				}
				return
			}
		}
		status.AppliedHash, status.RunningHash = desiredHash, runningHash
		status.FailedHash, status.LastFailureTime = "", nil
		conditions.True(&status.Conditions, generation, corev1alpha1.ConfigAppliedCondition, ReasonApplied, "")
		return
	}
	if status.FailedHash == desiredHash && status.LastFailureTime != nil && r.now().Sub(status.LastFailureTime.Time) < RetryInterval {
		// Same configuration, same router: it will be rejected again. The
		// condition keeps saying why.
		conditions.Set(&status.Conditions, generation, corev1alpha1.ConfigAppliedCondition, metav1.ConditionFalse,
			reasonOf(status.Conditions, ReasonApplyFailed), explain(status.Conditions, corev1alpha1.ConfigAppliedCondition))
		return
	}

	confirm := int(config.Spec.Management.ConfirmTimeoutMinutes)
	if confirm == 0 {
		confirm = 2
	}
	outcome, err := apply.Apply(ctx, api, desired, bootstrap.Keep, confirm)
	now := metav1.NewTime(r.now())
	if err != nil {
		status.FailedHash, status.LastFailureTime = desiredHash, &now
		var commitErr *apply.CommitError
		switch {
		case errors.As(err, &commitErr):
			notApplied(ReasonCommitFailed, err)
		case errors.Is(err, apply.ErrUnconfirmed):
			notApplied(ReasonUnconfirmed, err)
		default:
			notApplied(ReasonApplyFailed, err)
		}
		return
	}
	status.AppliedHash, status.RunningHash = desiredHash, outcome.RunningHash
	status.FailedHash, status.LastFailureTime = "", nil
	status.LastAppliedTime = &now
	conditions.True(&status.Conditions, generation, corev1alpha1.ConfigAppliedCondition, ReasonApplied, "")
}

func reasonOf(all []metav1.Condition, fallback string) string {
	if condition := conditions.Get(all, corev1alpha1.ConfigAppliedCondition); condition != nil && condition.Status == metav1.ConditionFalse {
		return condition.Reason
	}
	return fallback
}

func usesVRRP(paths []command.Path) bool {
	for _, path := range paths {
		if path.HasPrefix(command.Path{"high-availability", "vrrp"}) {
			return true
		}
	}
	return false
}

// observe reads the router's VRRP state and derives health and drain state
// from it.
func (r *Reconciler) observe(ctx context.Context, config *configv1alpha1.VyOSConfig, api *vyosclient.Client, redact func(string) string) {
	status := &config.Status
	generation := config.Generation

	output, err := api.Show(ctx, "vrrp")
	if err != nil {
		conditions.False(&status.Conditions, generation, corev1alpha1.HealthyCondition, ReasonUnreachable, redact(err.Error()))
		return
	}
	state, master, fault := vrrp.Summary(vrrp.Parse(output))
	// Active is the same fact in the terms core understands: it uses it to
	// take the standby first when it has a choice.
	for _, conditionType := range []string{configv1alpha1.VRRPStateCondition, corev1alpha1.ActiveCondition} {
		if master && !fault {
			conditions.True(&status.Conditions, generation, conditionType, state, "")
		} else {
			conditions.False(&status.Conditions, generation, conditionType, state, "")
		}
	}
	if fault {
		conditions.False(&status.Conditions, generation, corev1alpha1.HealthyCondition, ReasonVRRPFault, "a VRRP group is in FAULT state")
	} else {
		conditions.True(&status.Conditions, generation, corev1alpha1.HealthyCondition, ReasonHealthy, "")
	}

	_, draining := config.Annotations[corev1alpha1.DrainAnnotation]
	switch {
	case !draining:
		conditions.False(&status.Conditions, generation, corev1alpha1.DrainedCondition, ReasonNotDraining, "")
	case master:
		conditions.False(&status.Conditions, generation, corev1alpha1.DrainedCondition, ReasonDraining, "the router is still VRRP master")
	default:
		conditions.True(&status.Conditions, generation, corev1alpha1.DrainedCondition, ReasonDrained, "")
	}
}
