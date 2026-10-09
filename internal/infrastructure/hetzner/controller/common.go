// Package controller holds the controllers of the Hetzner Cloud
// infrastructure provider.
package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1alpha1 "github.com/hauke-cloud/router-api/api/infrastructure/v1alpha1"
)

// Condition reasons set by this provider.
const (
	ReasonReady                   = "Ready"
	ReasonTokenUnavailable        = "TokenUnavailable"
	ReasonNetworkNotFound         = "NetworkNotFound"
	ReasonNoManagementSources     = "NoManagementSources"
	ReasonCloudError              = "CloudError"
	ReasonInUse                   = "InUse"
	ReasonResolved                = "Resolved"
	ReasonResolutionFailed        = "ResolutionFailed"
	ReasonNetworkNotReady         = "NetworkNotReady"
	ReasonWaitingForOwner         = "WaitingForOwner"
	ReasonWaitingForBootstrapData = "WaitingForBootstrapData"
	ReasonServerRunning           = "ServerRunning"
	ReasonServerNotRunning        = "ServerNotRunning"
	ReasonServerNotFound          = "ServerNotFound"
	ReasonNameConflict            = "NameConflict"
	ReasonWaitingForPrimaryIP     = "WaitingForPrimaryIP"
	ReasonPrimaryIPUnusable       = "PrimaryIPUnusable"
	ReasonDeleting                = "Deleting"
	ReasonPaused                  = "Paused"
	ReasonExposureApplied         = "ExposureApplied"
	ReasonExposureNotFound        = "ExposureNotFound"
	ReasonExposureNotObserved     = "ExposureNotObserved"
)

// ManagementSourcesResolvedCondition is False on a HetznerRouterNetwork while
// a management source host name cannot be resolved and its last known
// addresses are used instead.
const ManagementSourcesResolvedCondition = "ManagementSourcesResolved"

// ExposureAppliedCondition is on a HetznerRouterNetwork whose firewall
// follows a RouterExposure: True while the firewall has that object's ports.
const ExposureAppliedCondition = "ExposureApplied"

const (
	// ManagedByLabel marks every Hetzner resource this provider creates.
	ManagedByLabel = "router-api.hauke.cloud/managed-by"
	// ManagedByValue is the value of ManagedByLabel.
	ManagedByValue = "router-api"
	// MachineUIDLabel ties a server to the HetznerMachine it was created
	// for. Names can be reused; UIDs are not.
	MachineUIDLabel = "router-api.hauke.cloud/machine-uid"
	// NetworkUIDLabel ties a firewall or placement group to its
	// HetznerRouterNetwork.
	NetworkUIDLabel = "router-api.hauke.cloud/network-uid"
)

const (
	// ResolveInterval is how often management source host names are
	// resolved again. It bounds how long the operator is locked out after
	// its address changed.
	ResolveInterval = time.Minute
	// pollInterval is how often a server that is not running yet is looked
	// at, and settledInterval one that is.
	pollInterval    = 10 * time.Second
	settledInterval = time.Minute
)

// resourceName is the name of the Hetzner firewall and placement group of a
// HetznerRouterNetwork. Hetzner names are unique per project, so the
// namespace is part of it.
func resourceName(network *infrav1alpha1.HetznerRouterNetwork) string {
	return "router-api-" + network.Namespace + "-" + network.Name
}

// ServerName is the name of the Hetzner server of a HetznerMachine.
func ServerName(machine *infrav1alpha1.HetznerMachine) string {
	return machine.Name
}

// token reads the API token a HetznerRouterNetwork refers to.
func token(ctx context.Context, c client.Reader, network *infrav1alpha1.HetznerRouterNetwork) (string, error) {
	ref := network.Spec.TokenSecretRef
	key := ref.Key
	if key == "" {
		key = "token"
	}
	secret := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: network.Namespace, Name: ref.Name}, secret); err != nil {
		return "", fmt.Errorf("get Secret %s: %w", ref.Name, err)
	}
	value := string(secret.Data[key])
	if value == "" {
		return "", fmt.Errorf("secret %s has no key %q", ref.Name, key)
	}
	return value, nil
}
