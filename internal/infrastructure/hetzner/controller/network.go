package controller

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1alpha1 "github.com/hauke-cloud/router-api/api/core/v1alpha1"
	infrav1alpha1 "github.com/hauke-cloud/router-api/api/infrastructure/v1alpha1"
	"github.com/hauke-cloud/router-api/internal/conditions"
	"github.com/hauke-cloud/router-api/internal/infrastructure/hetzner/cloud"
	"github.com/hauke-cloud/router-api/internal/pace"
)

// Resolver looks up the addresses of a host name.
type Resolver func(ctx context.Context, host string) ([]netip.Addr, error)

// DefaultResolver resolves through the system resolver.
func DefaultResolver(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// NetworkReconciler reconciles HetznerRouterNetworks: the firewall and the
// placement group a group of routers shares.
type NetworkReconciler struct {
	client.Client
	Cloud   cloud.Factory
	Resolve Resolver
}

// SetupWithManager registers the controller.
func (r *NetworkReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrav1alpha1.HetznerRouterNetwork{}).
		Watches(&corev1alpha1.RouterExposure{}, handler.EnqueueRequestsFromMapFunc(r.networksFor)).
		Complete(r)
}

// networksFor returns the networks whose firewall follows a RouterExposure.
func (r *NetworkReconciler) networksFor(ctx context.Context, exposure client.Object) []reconcile.Request {
	list := &infrav1alpha1.HetznerRouterNetworkList{}
	if err := r.List(ctx, list, client.InNamespace(exposure.GetNamespace())); err != nil {
		return nil
	}
	var requests []reconcile.Request
	for i := range list.Items {
		if ref := list.Items[i].Spec.Firewall.ExposureRef; ref != nil && ref.Name == exposure.GetName() {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
		}
	}
	return requests
}

// Reconcile brings one HetznerRouterNetwork up to date.
func (r *NetworkReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	network := &infrav1alpha1.HetznerRouterNetwork{}
	if err := r.Get(ctx, req.NamespacedName, network); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	if network.DeletionTimestamp.IsZero() && controllerutil.AddFinalizer(network, infrav1alpha1.HetznerRouterNetworkFinalizer) {
		if err := r.Update(ctx, network); err != nil {
			return reconcile.Result{}, err
		}
	}

	original := network.DeepCopy()
	var (
		result reconcile.Result
		err    error
	)
	if network.DeletionTimestamp.IsZero() {
		result, err = r.reconcileNormal(ctx, network)
	} else {
		result, err = r.reconcileDelete(ctx, network)
	}
	if !equality.Semantic.DeepEqual(network.Status, original.Status) {
		if statusErr := client.IgnoreNotFound(r.Status().Patch(ctx, network, client.MergeFrom(original))); statusErr != nil && err == nil {
			err = statusErr
		}
	}
	return result, err
}

func (r *NetworkReconciler) notReady(network *infrav1alpha1.HetznerRouterNetwork, reason, message string) {
	conditions.False(&network.Status.Conditions, network.Generation, corev1alpha1.ReadyCondition, reason, message)
}

func (r *NetworkReconciler) reconcileNormal(ctx context.Context, network *infrav1alpha1.HetznerRouterNetwork) (reconcile.Result, error) {
	status := &network.Status
	status.ObservedGeneration = network.Generation

	apiToken, tokenErr := token(ctx, r.Client, network)
	if tokenErr != nil {
		r.notReady(network, ReasonTokenUnavailable, tokenErr.Error())
		return reconcile.Result{RequeueAfter: pace.Every(pollInterval)}, nil //nolint:nilerr // reported as a condition
	}
	hcloud := r.Cloud(apiToken)

	networkID, err := hcloud.NetworkID(ctx, network.Spec.Network.Name)
	if errors.Is(err, cloud.ErrNotFound) {
		r.notReady(network, ReasonNetworkNotFound,
			fmt.Sprintf("Hetzner network %q does not exist; it is not created by this provider", network.Spec.Network.Name))
		return reconcile.Result{RequeueAfter: pace.Every(ResolveInterval)}, nil
	} else if err != nil {
		r.notReady(network, ReasonCloudError, err.Error())
		return reconcile.Result{}, err
	}
	status.NetworkID = networkID

	sources, hasHostnames := r.managementSources(ctx, network)
	if len(sources) == 0 {
		// A firewall without a management rule would make every router
		// built behind it unreachable for the operator.
		r.notReady(network, ReasonNoManagementSources, "no management source could be determined")
		return reconcile.Result{RequeueAfter: pace.Every(pollInterval)}, nil
	}
	status.ManagementCIDRs = make([]string, len(sources))
	for i, source := range sources {
		status.ManagementCIDRs[i] = source.String()
	}

	exposed, err := r.exposedPorts(ctx, network)
	if err != nil {
		return reconcile.Result{}, err
	}

	labels := map[string]string{ManagedByLabel: ManagedByValue, NetworkUIDLabel: string(network.UID)}
	rules := append(firewallRules(network, sources), exposedRules(exposed)...)
	firewallID, err := hcloud.EnsureFirewall(ctx, resourceName(network), labels, rules)
	if err != nil {
		r.notReady(network, ReasonCloudError, err.Error())
		return reconcile.Result{}, fmt.Errorf("ensure firewall: %w", err)
	}
	status.FirewallID = firewallID

	status.PlacementGroupID = 0
	if ptr.Deref(network.Spec.PlacementGroup.Enabled, true) {
		id, err := hcloud.EnsurePlacementGroup(ctx, resourceName(network), labels)
		if err != nil {
			r.notReady(network, ReasonCloudError, err.Error())
			return reconcile.Result{}, fmt.Errorf("ensure placement group: %w", err)
		}
		status.PlacementGroupID = id
	}

	conditions.True(&status.Conditions, network.Generation, corev1alpha1.ReadyCondition, ReasonReady, "")
	if hasHostnames {
		return reconcile.Result{RequeueAfter: pace.Every(ResolveInterval)}, nil
	}
	return reconcile.Result{RequeueAfter: pace.Every(10 * ResolveInterval)}, nil
}

// managementSources turns the configured management sources into prefixes,
// sorted and without duplicates. A host name that cannot be resolved keeps
// the addresses it had: losing them would lock the operator out of every
// router over a DNS hiccup.
func (r *NetworkReconciler) managementSources(ctx context.Context, network *infrav1alpha1.HetznerRouterNetwork) (sources []netip.Prefix, hasHostnames bool) {
	status := &network.Status
	resolve := r.Resolve
	if resolve == nil {
		resolve = DefaultResolver
	}

	var failed []string
	for _, source := range network.Spec.Firewall.ManagementSources {
		if source.CIDR != "" {
			if prefix, err := netip.ParsePrefix(source.CIDR); err == nil {
				sources = append(sources, prefix.Masked())
			} else {
				failed = append(failed, source.CIDR+" is not a CIDR")
			}
			continue
		}
		hasHostnames = true
		addrs, err := resolve(ctx, source.Hostname)
		if err != nil || len(addrs) == 0 {
			failed = append(failed, source.Hostname+" could not be resolved")
			continue
		}
		for _, addr := range addrs {
			addr = addr.Unmap()
			sources = append(sources, netip.PrefixFrom(addr, addr.BitLen()))
		}
	}

	if len(failed) > 0 {
		for _, previous := range status.ManagementCIDRs {
			if prefix, err := netip.ParsePrefix(previous); err == nil {
				sources = append(sources, prefix)
			}
		}
		conditions.False(&status.Conditions, network.Generation, ManagementSourcesResolvedCondition, ReasonResolutionFailed,
			fmt.Sprintf("%v; the previously allowed sources are kept", failed))
	} else {
		conditions.True(&status.Conditions, network.Generation, ManagementSourcesResolvedCondition, ReasonResolved, "")
	}

	slices.SortFunc(sources, func(a, b netip.Prefix) int {
		if c := a.Addr().Compare(b.Addr()); c != 0 {
			return c
		}
		return a.Bits() - b.Bits()
	})
	return slices.Compact(sources), hasHostnames
}

// exposedPorts returns the ports of the RouterExposure the firewall follows,
// and records in ExposureApplied whether there was one to follow. Without
// one the firewall is the management rule and the configured rules, never
// nothing: the operator's own access does not depend on a list of Gateways.
func (r *NetworkReconciler) exposedPorts(ctx context.Context, network *infrav1alpha1.HetznerRouterNetwork) ([]corev1alpha1.ExposedPort, error) {
	status := &network.Status
	ref := network.Spec.Firewall.ExposureRef
	if ref == nil {
		meta.RemoveStatusCondition(&status.Conditions, ExposureAppliedCondition)
		return nil, nil
	}
	exposure := &corev1alpha1.RouterExposure{}
	err := r.Get(ctx, client.ObjectKey{Namespace: network.Namespace, Name: ref.Name}, exposure)
	switch {
	case apierrors.IsNotFound(err):
		conditions.False(&status.Conditions, network.Generation, ExposureAppliedCondition, ReasonExposureNotFound,
			fmt.Sprintf("RouterExposure %q does not exist; only the configured rules are in effect", ref.Name))
		return nil, nil
	case err != nil:
		// Not knowing is no reason to close what is open.
		return nil, err
	case exposure.Status.ObservedGeneration == 0:
		conditions.False(&status.Conditions, network.Generation, ExposureAppliedCondition, ReasonExposureNotObserved,
			fmt.Sprintf("RouterExposure %q has not been computed yet; only the configured rules are in effect", ref.Name))
		return nil, nil
	}
	conditions.True(&status.Conditions, network.Generation, ExposureAppliedCondition, ReasonExposureApplied,
		fmt.Sprintf("%d port(s) from RouterExposure %q", len(exposure.Status.Ports), ref.Name))
	return exposure.Status.Ports, nil
}

// exposedRules turns exposed ports into rules, one per protocol and run of
// consecutive ports: Hetzner limits how many rules a firewall has.
func exposedRules(ports []corev1alpha1.ExposedPort) []cloud.FirewallRule {
	byProtocol := map[corev1alpha1.ExposedProtocol][]int32{}
	for _, port := range ports {
		byProtocol[port.Protocol] = append(byProtocol[port.Protocol], port.Port)
	}
	var rules []cloud.FirewallRule
	for _, protocol := range []corev1alpha1.ExposedProtocol{corev1alpha1.ExposedTCP, corev1alpha1.ExposedUDP} {
		numbers := byProtocol[protocol]
		slices.Sort(numbers)
		numbers = slices.Compact(numbers)
		for start := 0; start < len(numbers); {
			end := start
			for end+1 < len(numbers) && numbers[end+1] == numbers[end]+1 {
				end++
			}
			port := strconv.Itoa(int(numbers[start]))
			if end > start {
				port += "-" + strconv.Itoa(int(numbers[end]))
			}
			rules = append(rules, cloud.FirewallRule{
				Description: "router-api exposed by gateways",
				Protocol:    string(protocol),
				Port:        port,
				Sources:     everywhere,
			})
			start = end + 1
		}
	}
	return rules
}

var everywhere = []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0"), netip.MustParsePrefix("::/0")}

// firewallRules returns the management rule followed by the configured ones.
func firewallRules(network *infrav1alpha1.HetznerRouterNetwork, management []netip.Prefix) []cloud.FirewallRule {
	port := network.Spec.Firewall.ManagementPort
	if port == 0 {
		port = 443
	}
	rules := make([]cloud.FirewallRule, 0, 1+len(network.Spec.Firewall.Rules))
	rules = append(rules, cloud.FirewallRule{
		Description: "router-api management",
		Protocol:    "tcp",
		Port:        strconv.Itoa(int(port)),
		Sources:     management,
	})
	for _, rule := range network.Spec.Firewall.Rules {
		sources := make([]netip.Prefix, 0, len(rule.SourceCIDRs))
		for _, cidr := range rule.SourceCIDRs {
			if prefix, err := netip.ParsePrefix(cidr); err == nil {
				sources = append(sources, prefix.Masked())
			}
		}
		if len(rule.SourceCIDRs) == 0 {
			sources = everywhere
		}
		rules = append(rules, cloud.FirewallRule{
			Description: rule.Description,
			Protocol:    string(rule.Protocol),
			Port:        rule.Port,
			Sources:     sources,
		})
	}
	return rules
}

func (r *NetworkReconciler) reconcileDelete(ctx context.Context, network *infrav1alpha1.HetznerRouterNetwork) (reconcile.Result, error) {
	if !controllerutil.ContainsFinalizer(network, infrav1alpha1.HetznerRouterNetworkFinalizer) {
		return reconcile.Result{}, nil
	}

	// The machines need this object to find the token their server is
	// deleted with. It stays until the last of them is gone.
	machines := &infrav1alpha1.HetznerMachineList{}
	if err := r.List(ctx, machines, client.InNamespace(network.Namespace)); err != nil {
		return reconcile.Result{}, err
	}
	users := 0
	for i := range machines.Items {
		if machines.Items[i].Spec.NetworkRef.Name == network.Name {
			users++
		}
	}
	if users > 0 {
		r.notReady(network, ReasonInUse, fmt.Sprintf("waiting for %d HetznerMachine(s) that use this network to be deleted", users))
		return reconcile.Result{RequeueAfter: pace.Every(pollInterval)}, nil
	}

	apiToken, tokenErr := token(ctx, r.Client, network)
	if tokenErr != nil {
		// Waited for rather than failed on: the Secret may simply be
		// restored, and nothing can be cleaned up without it.
		r.notReady(network, ReasonTokenUnavailable, "cannot delete the firewall and placement group: "+tokenErr.Error())
		return reconcile.Result{RequeueAfter: pace.Every(pollInterval)}, nil //nolint:nilerr // reported as a condition
	}
	hcloud := r.Cloud(apiToken)
	for _, remove := range []func(context.Context, string) error{hcloud.DeleteFirewall, hcloud.DeletePlacementGroup} {
		if err := remove(ctx, resourceName(network)); errors.Is(err, cloud.ErrInUse) {
			// Hetzner detaches a deleted server's firewall a moment later.
			r.notReady(network, ReasonInUse, err.Error())
			return reconcile.Result{RequeueAfter: pace.Every(pollInterval)}, nil
		} else if err != nil {
			r.notReady(network, ReasonCloudError, err.Error())
			return reconcile.Result{}, err
		}
	}

	controllerutil.RemoveFinalizer(network, infrav1alpha1.HetznerRouterNetworkFinalizer)
	return reconcile.Result{}, r.Update(ctx, network)
}
