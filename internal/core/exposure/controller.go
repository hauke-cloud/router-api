// Package exposure reconciles RouterExposures: it collects, from the Gateways
// that ask for it and the ListenerSets attached to them, the addresses and
// ports a group of routers is to let through, for the providers to act on.
package exposure

import (
	"cmp"
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	corev1alpha1 "github.com/hauke-cloud/router-api/api/core/v1alpha1"
	"github.com/hauke-cloud/router-api/internal/conditions"
	"github.com/hauke-cloud/router-api/internal/pace"
)

// Condition reasons set by this controller.
const (
	ReasonCollected             = "Collected"
	ReasonGatewayAPIUnavailable = "GatewayAPIUnavailable"
)

// resyncInterval is how often the Gateways are looked at without a reason.
// It is also what picks them up at all if the Gateway API was installed
// after this manager started, when there is no watch.
const resyncInterval = time.Minute

// Reconciler reconciles RouterExposures.
type Reconciler struct {
	client.Client
}

// SetupWithManager registers the controller. Gateways and ListenerSets are
// only watched if the cluster serves them: an informer for a kind the API
// server does not know fails rather than waits, and router-api has to work in
// a cluster without the Gateway API, or with one from before ListenerSets.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	builder := ctrl.NewControllerManagedBy(mgr).For(&corev1alpha1.RouterExposure{})
	for _, watched := range []struct {
		kind   string
		object client.Object
		mapper handler.MapFunc
	}{
		{"Gateway", &gatewayv1.Gateway{}, exposureFor},
		{"ListenerSet", &gatewayv1.ListenerSet{}, r.exposureForListenerSet},
	} {
		kind := schema.GroupKind{Group: gatewayv1.GroupName, Kind: watched.kind}
		if _, err := mgr.GetRESTMapper().RESTMapping(kind, gatewayv1.GroupVersion.Version); err != nil {
			mgr.GetLogger().Info("the cluster does not serve "+watched.kind+"s; they are looked for periodically instead of watched",
				"kind", kind.String(), "error", err.Error())
			continue
		}
		builder = builder.Watches(watched.object, handler.EnqueueRequestsFromMapFunc(watched.mapper))
	}
	return builder.Complete(r)
}

// target returns the RouterExposure a Gateway names, if it names one.
func target(gateway client.Object) (types.NamespacedName, bool) {
	value := strings.TrimSpace(gateway.GetAnnotations()[corev1alpha1.ExposeAnnotation])
	if value == "" {
		return types.NamespacedName{}, false
	}
	namespace, name, qualified := strings.Cut(value, "/")
	if !qualified {
		return types.NamespacedName{Namespace: gateway.GetNamespace(), Name: value}, true
	}
	if namespace == "" || name == "" {
		return types.NamespacedName{}, false
	}
	return types.NamespacedName{Namespace: namespace, Name: name}, true
}

// exposureFor is called with the old and the new Gateway of an update, so a
// Gateway that stops naming a RouterExposure wakes the one it leaves.
func exposureFor(_ context.Context, gateway client.Object) []reconcile.Request {
	key, ok := target(gateway)
	if !ok {
		return nil
	}
	return []reconcile.Request{{NamespacedName: key}}
}

// parent returns the Gateway a ListenerSet attaches to, if it is a Gateway.
func parent(set *gatewayv1.ListenerSet) (types.NamespacedName, bool) {
	ref := set.Spec.ParentRef
	if (ref.Group != nil && *ref.Group != gatewayv1.GroupName) || (ref.Kind != nil && *ref.Kind != "Gateway") {
		return types.NamespacedName{}, false
	}
	key := types.NamespacedName{Namespace: set.Namespace, Name: string(ref.Name)}
	if ref.Namespace != nil && *ref.Namespace != "" {
		key.Namespace = string(*ref.Namespace)
	}
	return key, true
}

// exposureForListenerSet wakes the RouterExposure the ListenerSet's Gateway
// names. A Gateway that cannot be read wakes nothing: the periodic look
// makes up for it.
func (r *Reconciler) exposureForListenerSet(ctx context.Context, object client.Object) []reconcile.Request {
	set, ok := object.(*gatewayv1.ListenerSet)
	if !ok {
		return nil
	}
	key, ok := parent(set)
	if !ok {
		return nil
	}
	gateway := &gatewayv1.Gateway{}
	if err := r.Get(ctx, key, gateway); err != nil {
		return nil
	}
	return exposureFor(ctx, gateway)
}

// Reconcile collects what one RouterExposure exposes.
func (r *Reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	exposure := &corev1alpha1.RouterExposure{}
	if err := r.Get(ctx, req.NamespacedName, exposure); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if !exposure.DeletionTimestamp.IsZero() {
		return reconcile.Result{}, nil
	}
	if _, paused := exposure.Annotations[corev1alpha1.PausedAnnotation]; paused {
		return reconcile.Result{}, nil
	}

	original := exposure.DeepCopy()
	result, err := r.reconcile(ctx, exposure)
	if !equality.Semantic.DeepEqual(exposure.Status, original.Status) {
		if statusErr := client.IgnoreNotFound(r.Status().Patch(ctx, exposure, client.MergeFrom(original))); statusErr != nil && err == nil {
			err = statusErr
		}
	}
	return result, err
}

func (r *Reconciler) reconcile(ctx context.Context, exposure *corev1alpha1.RouterExposure) (reconcile.Result, error) {
	status := &exposure.Status

	gateways := &gatewayv1.GatewayList{}
	if err := r.List(ctx, gateways); err != nil {
		if !meta.IsNoMatchError(err) {
			// Nothing is known, so nothing is changed: what was exposed
			// stays exposed.
			return reconcile.Result{}, err
		}
		// No Gateway API, no Gateways. The same answer comes back while a
		// CustomResourceDefinition is being replaced, though, and closing
		// every port over that would be worse than keeping them a little
		// too long.
		conditions.False(&status.Conditions, exposure.Generation, corev1alpha1.ReadyCondition, ReasonGatewayAPIUnavailable,
			"the cluster does not serve gateway.networking.k8s.io Gateways; what was exposed before is kept")
		return reconcile.Result{RequeueAfter: pace.Every(resyncInterval)}, nil
	}

	listenerSets := &gatewayv1.ListenerSetList{}
	if err := r.List(ctx, listenerSets); err != nil {
		if !meta.IsNoMatchError(err) {
			return reconcile.Result{}, err
		}
		// A Gateway API from before ListenerSets has none, and the Gateways
		// are all there is. If there were some before, though, this is the
		// CustomResourceDefinition being replaced, as above.
		if len(status.ListenerSets) > 0 {
			conditions.False(&status.Conditions, exposure.Generation, corev1alpha1.ReadyCondition, ReasonGatewayAPIUnavailable,
				"the cluster does not serve gateway.networking.k8s.io ListenerSets any more; what was exposed before is kept")
			return reconcile.Result{RequeueAfter: pace.Every(resyncInterval)}, nil
		}
	}
	attached := map[types.NamespacedName][]*gatewayv1.ListenerSet{}
	for i := range listenerSets.Items {
		set := &listenerSets.Items[i]
		if gateway, ok := parent(set); ok && set.DeletionTimestamp.IsZero() {
			attached[gateway] = append(attached[gateway], set)
		}
	}

	key := client.ObjectKeyFromObject(exposure)
	ports := map[netip.Addr]map[corev1alpha1.ExposedPort]struct{}{}
	status.Gateways, status.ListenerSets = nil, nil
	for i := range gateways.Items {
		gateway := &gateways.Items[i]
		if named, ok := target(gateway); !ok || named != key || !gateway.DeletionTimestamp.IsZero() {
			continue
		}
		report := corev1alpha1.ExposedGateway{Namespace: gateway.Namespace, Name: gateway.Name}
		sets := attached[client.ObjectKeyFromObject(gateway)]
		if allowed(exposure, gateway.Namespace) {
			addresses, unaddressed := gatewayAddresses(gateway)
			listeners, notes := exposedPorts(gateway.Spec.Listeners, func(l gatewayv1.Listener) (gatewayv1.SectionName, gatewayv1.ProtocolType, int32) {
				return l.Name, l.Protocol, l.Port
			})
			for _, set := range sets {
				setReport := listenerSetReport(set)
				// Accepted is the Gateway's implementation saying that the
				// Gateway's spec.allowedListeners selects the ListenerSet.
				// Until it does, the listeners are not the Gateway's.
				if accepted := meta.FindStatusCondition(set.Status.Conditions, string(gatewayv1.ListenerSetConditionAccepted)); accepted == nil || accepted.Status != metav1.ConditionTrue {
					setReport.Message = "the Gateway has not accepted the ListenerSet"
					if accepted != nil && accepted.Reason != "" {
						setReport.Message += ": " + accepted.Reason
					}
				} else {
					setPorts, setNotes := exposedPorts(set.Spec.Listeners, func(l gatewayv1.ListenerEntry) (gatewayv1.SectionName, gatewayv1.ProtocolType, int32) {
						return l.Name, l.Protocol, l.Port
					})
					listeners = append(listeners, setPorts...)
					setReport.Exposed = len(addresses) > 0 && len(setPorts) > 0
					setReport.Message = strings.Join(append(setNotes, unaddressed...), "; ")
				}
				status.ListenerSets = append(status.ListenerSets, setReport)
			}
			if len(listeners) == 0 {
				notes = append(notes, "no listener with a port to expose")
			}
			notes = append(notes, unaddressed...)
			for _, address := range addresses {
				if ports[address] == nil {
					ports[address] = map[corev1alpha1.ExposedPort]struct{}{}
				}
				for _, port := range listeners {
					ports[address][port] = struct{}{}
				}
			}
			report.Exposed = len(addresses) > 0 && len(listeners) > 0
			report.Message = strings.Join(notes, "; ")
		} else {
			report.Message = fmt.Sprintf("namespace %s is not in spec.gateways.namespaces", gateway.Namespace)
			for _, set := range sets {
				setReport := listenerSetReport(set)
				setReport.Message = fmt.Sprintf("its Gateway is not exposed: namespace %s is not in spec.gateways.namespaces", gateway.Namespace)
				status.ListenerSets = append(status.ListenerSets, setReport)
			}
		}
		status.Gateways = append(status.Gateways, report)
	}
	slices.SortFunc(status.Gateways, func(a, b corev1alpha1.ExposedGateway) int {
		return cmp.Or(strings.Compare(a.Namespace, b.Namespace), strings.Compare(a.Name, b.Name))
	})
	slices.SortFunc(status.ListenerSets, func(a, b corev1alpha1.ExposedListenerSet) int {
		return cmp.Or(strings.Compare(a.Namespace, b.Namespace), strings.Compare(a.Name, b.Name))
	})

	status.Endpoints, status.Ports = nil, nil
	union := map[corev1alpha1.ExposedPort]struct{}{}
	for _, address := range slices.SortedFunc(mapKeys(ports), netip.Addr.Compare) {
		if len(ports[address]) == 0 {
			continue
		}
		for port := range ports[address] {
			union[port] = struct{}{}
		}
		status.Endpoints = append(status.Endpoints, corev1alpha1.ExposedEndpoint{
			Address: address.String(),
			Ports:   slices.SortedFunc(mapKeys(ports[address]), comparePorts),
		})
	}
	if len(union) > 0 {
		status.Ports = slices.SortedFunc(mapKeys(union), comparePorts)
	}

	status.ObservedGeneration = exposure.Generation
	conditions.True(&status.Conditions, exposure.Generation, corev1alpha1.ReadyCondition, ReasonCollected, "")
	return reconcile.Result{RequeueAfter: pace.Every(resyncInterval)}, nil
}

func mapKeys[K comparable, V any](m map[K]V) func(yield func(K) bool) {
	return func(yield func(K) bool) {
		for key := range m {
			if !yield(key) {
				return
			}
		}
	}
}

func comparePorts(a, b corev1alpha1.ExposedPort) int {
	return cmp.Or(strings.Compare(string(a.Protocol), string(b.Protocol)), cmp.Compare(a.Port, b.Port))
}

// allowed reports whether Gateways of a namespace may contribute.
func allowed(exposure *corev1alpha1.RouterExposure, namespace string) bool {
	return namespace == exposure.Namespace ||
		slices.Contains(exposure.Spec.Gateways.Namespaces, namespace) ||
		slices.Contains(exposure.Spec.Gateways.Namespaces, corev1alpha1.AllNamespaces)
}

// listenerSetReport starts what the status says about a ListenerSet.
func listenerSetReport(set *gatewayv1.ListenerSet) corev1alpha1.ExposedListenerSet {
	gateway, _ := parent(set)
	return corev1alpha1.ExposedListenerSet{Namespace: set.Namespace, Name: set.Name, Gateway: gateway.Namespace + "/" + gateway.Name}
}

// exposedPorts returns the ports of listeners, a Gateway's or a
// ListenerSet's. notes say what was left out.
func exposedPorts[L any](listeners []L, of func(L) (gatewayv1.SectionName, gatewayv1.ProtocolType, int32)) (ports []corev1alpha1.ExposedPort, notes []string) {
	for _, listener := range listeners {
		name, listenerProtocol, port := of(listener)
		var protocol corev1alpha1.ExposedProtocol
		switch listenerProtocol {
		case gatewayv1.HTTPProtocolType, gatewayv1.HTTPSProtocolType, gatewayv1.TLSProtocolType, gatewayv1.TCPProtocolType:
			protocol = corev1alpha1.ExposedTCP
		case gatewayv1.UDPProtocolType:
			protocol = corev1alpha1.ExposedUDP
		default:
			// An implementation's own protocol. What it is carried by is
			// not ours to guess.
			notes = append(notes, fmt.Sprintf("listener %s has protocol %s, which is not one of HTTP, HTTPS, TLS, TCP and UDP", name, listenerProtocol))
			continue
		}
		ports = append(ports, corev1alpha1.ExposedPort{Protocol: protocol, Port: port})
	}
	return ports, notes
}

// gatewayAddresses returns the IP addresses a Gateway has been given. notes say
// that there are none.
func gatewayAddresses(gateway *gatewayv1.Gateway) (addresses []netip.Addr, notes []string) {
	// The status, not the spec: an address that was asked for is not yet
	// one the Gateway answers on.
	for _, address := range gateway.Status.Addresses {
		if address.Type != nil && *address.Type != gatewayv1.IPAddressType {
			continue
		}
		if parsed, err := netip.ParseAddr(address.Value); err == nil {
			addresses = append(addresses, parsed.Unmap())
		}
	}
	if len(addresses) == 0 {
		notes = append(notes, "the Gateway has no IP address in its status yet")
	}
	return addresses, notes
}
