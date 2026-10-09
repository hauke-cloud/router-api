// Package exposure reconciles RouterExposures: it collects, from the Gateways
// that ask for it, the addresses and ports a group of routers is to let
// through, for the providers to act on.
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

// SetupWithManager registers the controller. Gateways are only watched if
// the cluster serves them: an informer for a kind the API server does not
// know fails rather than waits, and router-api has to work in a cluster
// without the Gateway API.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	builder := ctrl.NewControllerManagedBy(mgr).For(&corev1alpha1.RouterExposure{})
	gateways := schema.GroupKind{Group: gatewayv1.GroupName, Kind: "Gateway"}
	if _, err := mgr.GetRESTMapper().RESTMapping(gateways, gatewayv1.GroupVersion.Version); err == nil {
		builder = builder.Watches(&gatewayv1.Gateway{}, handler.EnqueueRequestsFromMapFunc(exposureFor))
	} else {
		mgr.GetLogger().Info("the cluster does not serve Gateways; they are looked for periodically instead of watched",
			"kind", gateways.String(), "error", err.Error())
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

	key := client.ObjectKeyFromObject(exposure)
	ports := map[netip.Addr]map[corev1alpha1.ExposedPort]struct{}{}
	status.Gateways = nil
	for i := range gateways.Items {
		gateway := &gateways.Items[i]
		if named, ok := target(gateway); !ok || named != key || !gateway.DeletionTimestamp.IsZero() {
			continue
		}
		report := corev1alpha1.ExposedGateway{Namespace: gateway.Namespace, Name: gateway.Name}
		if allowed(exposure, gateway.Namespace) {
			addresses, listeners, notes := endpoints(gateway)
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
		}
		status.Gateways = append(status.Gateways, report)
	}
	slices.SortFunc(status.Gateways, func(a, b corev1alpha1.ExposedGateway) int {
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

// endpoints returns the IP addresses a Gateway has been given and the ports
// of its listeners. notes say what was left out.
func endpoints(gateway *gatewayv1.Gateway) (addresses []netip.Addr, ports []corev1alpha1.ExposedPort, notes []string) {
	for _, listener := range gateway.Spec.Listeners {
		var protocol corev1alpha1.ExposedProtocol
		switch listener.Protocol {
		case gatewayv1.HTTPProtocolType, gatewayv1.HTTPSProtocolType, gatewayv1.TLSProtocolType, gatewayv1.TCPProtocolType:
			protocol = corev1alpha1.ExposedTCP
		case gatewayv1.UDPProtocolType:
			protocol = corev1alpha1.ExposedUDP
		default:
			// An implementation's own protocol. What it is carried by is
			// not ours to guess.
			notes = append(notes, fmt.Sprintf("listener %s has protocol %s, which is not one of HTTP, HTTPS, TLS, TCP and UDP", listener.Name, listener.Protocol))
			continue
		}
		ports = append(ports, corev1alpha1.ExposedPort{Protocol: protocol, Port: listener.Port})
	}
	if len(ports) == 0 {
		notes = append(notes, "no listener with a port to expose")
	}

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
	return addresses, ports, notes
}
