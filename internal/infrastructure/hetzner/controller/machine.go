package controller

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1alpha1 "github.com/hauke-cloud/router-api/api/core/v1alpha1"
	infrav1alpha1 "github.com/hauke-cloud/router-api/api/infrastructure/v1alpha1"
	"github.com/hauke-cloud/router-api/internal/conditions"
	"github.com/hauke-cloud/router-api/internal/contract"
	"github.com/hauke-cloud/router-api/internal/infrastructure/hetzner/cloud"
	"github.com/hauke-cloud/router-api/internal/pace"
)

// MachineReconciler reconciles HetznerMachines: one Hetzner Cloud server
// each.
type MachineReconciler struct {
	client.Client
	Cloud cloud.Factory
}

// SetupWithManager registers the controller.
func (r *MachineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrav1alpha1.HetznerMachine{}).
		Complete(r)
}

// Reconcile brings one HetznerMachine up to date.
func (r *MachineReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	machine := &infrav1alpha1.HetznerMachine{}
	if err := r.Get(ctx, req.NamespacedName, machine); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	original := machine.DeepCopy()
	var (
		result reconcile.Result
		err    error
	)
	if machine.DeletionTimestamp.IsZero() {
		result, err = r.reconcileNormal(ctx, machine)
	} else {
		result, err = r.reconcileDelete(ctx, machine)
	}
	if !equality.Semantic.DeepEqual(machine.Status, original.Status) {
		base := machine.DeepCopy()
		base.Status = original.Status
		if statusErr := client.IgnoreNotFound(r.Status().Patch(ctx, machine, client.MergeFrom(base))); statusErr != nil && err == nil {
			err = statusErr
		}
	}
	return result, err
}

func notReady(machine *infrav1alpha1.HetznerMachine, reason, message string) {
	conditions.False(&machine.Status.Conditions, machine.Generation, corev1alpha1.ReadyCondition, reason, message)
}

// update writes metadata and spec, keeping the status that is being built up
// in memory.
func (r *MachineReconciler) update(ctx context.Context, machine *infrav1alpha1.HetznerMachine) error {
	status := machine.Status
	if err := r.Update(ctx, machine); err != nil {
		return err
	}
	machine.Status = status
	return nil
}

// network returns the machine's HetznerRouterNetwork and a client for its
// project, or a description of why that is not possible yet.
func (r *MachineReconciler) network(ctx context.Context, machine *infrav1alpha1.HetznerMachine) (*infrav1alpha1.HetznerRouterNetwork, cloud.Cloud, string) {
	network := &infrav1alpha1.HetznerRouterNetwork{}
	err := r.Get(ctx, types.NamespacedName{Namespace: machine.Namespace, Name: machine.Spec.NetworkRef.Name}, network)
	if err != nil {
		return nil, nil, fmt.Sprintf("HetznerRouterNetwork %s: %v", machine.Spec.NetworkRef.Name, err)
	}
	apiToken, err := token(ctx, r.Client, network)
	if err != nil {
		return nil, nil, err.Error()
	}
	return network, r.Cloud(apiToken), ""
}

func (r *MachineReconciler) reconcileNormal(ctx context.Context, machine *infrav1alpha1.HetznerMachine) (reconcile.Result, error) {
	status := &machine.Status
	status.ObservedGeneration = machine.Generation

	// The owning RouterMachine says what the server boots with.
	owner, err := r.owner(ctx, machine)
	if err != nil {
		return reconcile.Result{}, err
	}
	if owner == nil {
		notReady(machine, ReasonWaitingForOwner, "not owned by a RouterMachine yet")
		return reconcile.Result{RequeueAfter: pace.Every(pollInterval)}, nil
	}
	if _, paused := owner.Annotations[corev1alpha1.PausedAnnotation]; paused {
		return reconcile.Result{}, nil
	}
	if _, paused := machine.Annotations[corev1alpha1.PausedAnnotation]; paused {
		return reconcile.Result{}, nil
	}

	network, hcloud, problem := r.network(ctx, machine)
	if problem == "" && !conditions.IsTrue(network.Status.Conditions, corev1alpha1.ReadyCondition) {
		problem = fmt.Sprintf("HetznerRouterNetwork %s is not ready", network.Name)
	}
	if problem != "" {
		notReady(machine, ReasonNetworkNotReady, problem)
		return reconcile.Result{RequeueAfter: pace.Every(pollInterval)}, nil
	}

	if controllerutil.AddFinalizer(machine, infrav1alpha1.HetznerMachineFinalizer) {
		if err := r.update(ctx, machine); err != nil {
			return reconcile.Result{}, err
		}
		status = &machine.Status
	}

	server, err := hcloud.ServerByName(ctx, ServerName(machine))
	switch {
	case errors.Is(err, cloud.ErrNotFound):
		server = nil
	case err != nil:
		notReady(machine, ReasonCloudError, err.Error())
		return reconcile.Result{}, err
	case server.Labels[MachineUIDLabel] != string(machine.UID):
		notReady(machine, ReasonNameConflict,
			fmt.Sprintf("a server named %s exists and was not created for this object", server.Name))
		return reconcile.Result{RequeueAfter: pace.Every(settledInterval)}, nil
	}

	if server == nil {
		if machine.Spec.ProviderID != "" {
			// There was a server and it is gone. A new one from the same
			// user data would come up as a router the config provider
			// believes it has already configured; whether to replace the
			// router is not this controller's decision.
			status.ServerStatus = ""
			notReady(machine, ReasonServerNotFound, fmt.Sprintf("server %s no longer exists", machine.Spec.ProviderID))
			return reconcile.Result{RequeueAfter: pace.Every(settledInterval)}, nil
		}
		server, err = r.create(ctx, machine, owner, network, hcloud)
		if err != nil {
			return reconcile.Result{}, err
		}
		if server == nil {
			return reconcile.Result{RequeueAfter: pace.Every(pollInterval)}, nil
		}
	}

	if providerID := infrav1alpha1.ProviderIDPrefix + strconv.FormatInt(server.ID, 10); machine.Spec.ProviderID != providerID {
		machine.Spec.ProviderID = providerID
		if err := r.update(ctx, machine); err != nil {
			return reconcile.Result{}, err
		}
		status = &machine.Status
	}

	if err := r.remediate(ctx, machine, server, hcloud); err != nil {
		return reconcile.Result{}, err
	}

	status.ServerStatus = server.Status
	status.FailureDomain = server.Location
	status.Addresses = addresses(server)
	if server.Status != cloud.ServerStatusRunning {
		notReady(machine, ReasonServerNotRunning, "the server is "+server.Status)
		return reconcile.Result{RequeueAfter: pace.Every(pollInterval)}, nil
	}
	status.Initialization = &infrav1alpha1.HetznerMachineInitialization{Provisioned: ptr.To(true)}
	conditions.True(&status.Conditions, machine.Generation, corev1alpha1.ReadyCondition, ReasonServerRunning, "")
	return reconcile.Result{RequeueAfter: pace.Every(settledInterval)}, nil
}

func (r *MachineReconciler) owner(ctx context.Context, machine *infrav1alpha1.HetznerMachine) (*corev1alpha1.RouterMachine, error) {
	ref := metav1.GetControllerOf(machine)
	if ref == nil || ref.Kind != "RouterMachine" {
		return nil, nil
	}
	owner := &corev1alpha1.RouterMachine{}
	err := r.Get(ctx, types.NamespacedName{Namespace: machine.Namespace, Name: ref.Name}, owner)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	return owner, err
}

// create creates the server. It returns nil without an error when it has to
// wait for something, and records what in the Ready condition.
func (r *MachineReconciler) create(ctx context.Context, machine *infrav1alpha1.HetznerMachine, owner *corev1alpha1.RouterMachine,
	network *infrav1alpha1.HetznerRouterNetwork, hcloud cloud.Cloud) (*cloud.Server, error) {
	secretName := ptr.Deref(owner.Spec.Bootstrap.DataSecretName, "")
	if secretName == "" {
		notReady(machine, ReasonWaitingForBootstrapData, "the config provider has not published the data the server boots with")
		return nil, nil
	}
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: machine.Namespace, Name: secretName}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			notReady(machine, ReasonWaitingForBootstrapData, fmt.Sprintf("Secret %s does not exist", secretName))
			return nil, nil
		}
		return nil, err
	}
	userData := string(secret.Data[contract.BootstrapDataKey])
	if userData == "" {
		notReady(machine, ReasonWaitingForBootstrapData, fmt.Sprintf("Secret %s has no key %q", secretName, contract.BootstrapDataKey))
		return nil, nil
	}

	primaryIPv4, ok, err := r.primaryIPv4(ctx, machine, hcloud)
	if err != nil || !ok {
		return nil, err
	}

	image := machine.Spec.Image
	if image == "" {
		image = "ubuntu-24.04"
	}
	server, err := hcloud.CreateServer(ctx, &cloud.ServerSpec{
		PrimaryIPv4ID: primaryIPv4,
		Name:          ServerName(machine),
		ServerType:    machine.Spec.ServerType,
		Location:      machine.Spec.Location,
		Image:         image,
		UserData:      userData,
		Labels: map[string]string{
			ManagedByLabel:  ManagedByValue,
			MachineUIDLabel: string(machine.UID),
			NetworkUIDLabel: string(network.UID),
		},
		SSHKeys:          network.Spec.SSHKeys,
		NetworkID:        network.Status.NetworkID,
		FirewallID:       network.Status.FirewallID,
		PlacementGroupID: network.Status.PlacementGroupID,
		EnableIPv4:       ptr.Deref(machine.Spec.EnableIPv4, true),
		EnableIPv6:       ptr.Deref(machine.Spec.EnableIPv6, true),
	})
	if err != nil {
		notReady(machine, ReasonCloudError, err.Error())
		return nil, fmt.Errorf("create server: %w", err)
	}
	return server, nil
}

// primaryIPv4 returns the ID of the Primary IP the machine's slot is to have,
// or 0 if it is to get an address of its own. ok is false when the server
// cannot be created yet, or not at all as specified; the Ready condition
// then says why.
func (r *MachineReconciler) primaryIPv4(ctx context.Context, machine *infrav1alpha1.HetznerMachine, hcloud cloud.Cloud) (id int64, ok bool, err error) {
	names := machine.Spec.PrimaryIPv4BySlot
	if len(names) == 0 {
		return 0, true, nil
	}
	unusable := func(format string, args ...any) (int64, bool, error) {
		notReady(machine, ReasonPrimaryIPUnusable, fmt.Sprintf(format, args...))
		return 0, false, nil
	}

	label, labelled := machine.Labels[corev1alpha1.SlotLabel]
	slot, convErr := strconv.Atoi(label)
	if !labelled || convErr != nil || slot < 0 {
		return unusable("primaryIPv4BySlot is set, but this machine has no slot; it needs a RouterDeployment with the Slots strategy")
	}
	if slot >= len(names) {
		return unusable("primaryIPv4BySlot names %d addresses and this machine is in slot %d", len(names), slot)
	}

	primary, err := hcloud.PrimaryIPv4(ctx, names[slot])
	switch {
	case errors.Is(err, cloud.ErrNotFound):
		return unusable("Hetzner has no IPv4 Primary IP named %q; it is not created by this provider", names[slot])
	case err != nil:
		notReady(machine, ReasonCloudError, err.Error())
		return 0, false, err
	case primary.AutoDelete:
		// It would not survive the first server it is given to, and the
		// address everyone was told to dial would go back to Hetzner.
		return unusable("Primary IP %q has auto-delete on and would be deleted with the server; turn it off", primary.Name)
	case primary.AssigneeID != 0:
		// Normal during a replacement: the server of the router this one
		// succeeds is still on its way out.
		notReady(machine, ReasonWaitingForPrimaryIP,
			fmt.Sprintf("Primary IP %q is still assigned to server %d", primary.Name, primary.AssigneeID))
		return 0, false, nil
	}
	return primary.ID, true, nil
}

// remediate acts on a remediation request once. The request is the value of
// the annotation; recording it as handled is the answer to whoever asked.
func (r *MachineReconciler) remediate(ctx context.Context, machine *infrav1alpha1.HetznerMachine, server *cloud.Server, hcloud cloud.Cloud) error {
	request, ok := machine.Annotations[corev1alpha1.RemediationAnnotation]
	if !ok || request == machine.Status.LastRemediation {
		return nil
	}
	if action, _, _ := strings.Cut(request, "/"); action == string(corev1alpha1.RemediationReboot) {
		if err := hcloud.ResetServer(ctx, server.ID); err != nil {
			return fmt.Errorf("reset server: %w", err)
		}
	}
	// Unknown actions are recorded as handled too: retrying them for ever
	// would not make them known.
	machine.Status.LastRemediation = request
	return nil
}

func addresses(server *cloud.Server) []corev1alpha1.MachineAddress {
	out := []corev1alpha1.MachineAddress{{Type: corev1alpha1.AddressHostname, Address: server.Name}}
	if server.PublicIPv4.IsValid() {
		out = append(out, corev1alpha1.MachineAddress{Type: corev1alpha1.AddressExternalIP, Address: server.PublicIPv4.String()})
	}
	if server.PublicIPv6.IsValid() {
		out = append(out, corev1alpha1.MachineAddress{Type: corev1alpha1.AddressExternalIP, Address: server.PublicIPv6.String()})
	}
	for _, ip := range server.PrivateIPs {
		out = append(out, corev1alpha1.MachineAddress{Type: corev1alpha1.AddressInternalIP, Address: ip.String()})
	}
	return out
}

func (r *MachineReconciler) reconcileDelete(ctx context.Context, machine *infrav1alpha1.HetznerMachine) (reconcile.Result, error) {
	if !controllerutil.ContainsFinalizer(machine, infrav1alpha1.HetznerMachineFinalizer) {
		return reconcile.Result{}, nil
	}

	_, hcloud, problem := r.network(ctx, machine)
	if problem != "" {
		// No token, no way to delete the server. Letting go of the object
		// now would leave it running and billed with nothing in the cluster
		// that knows about it.
		notReady(machine, ReasonNetworkNotReady, "cannot delete the server: "+problem)
		return reconcile.Result{RequeueAfter: pace.Every(pollInterval)}, nil
	}

	server, err := hcloud.ServerByName(ctx, ServerName(machine))
	switch {
	case errors.Is(err, cloud.ErrNotFound):
		// Gone.
	case err != nil:
		return reconcile.Result{}, err
	case server.Labels[MachineUIDLabel] != string(machine.UID):
		// Not ours. Leave it alone.
	default:
		if err := hcloud.DeleteServer(ctx, server.ID); err != nil {
			notReady(machine, ReasonCloudError, err.Error())
			return reconcile.Result{}, fmt.Errorf("delete server: %w", err)
		}
		// Deletion is asynchronous at Hetzner. Look again before letting go.
		notReady(machine, ReasonDeleting, "waiting for the server to be deleted")
		if _, err := hcloud.ServerByName(ctx, ServerName(machine)); !errors.Is(err, cloud.ErrNotFound) {
			return reconcile.Result{RequeueAfter: pace.Every(pollInterval)}, client.IgnoreNotFound(err)
		}
	}

	controllerutil.RemoveFinalizer(machine, infrav1alpha1.HetznerMachineFinalizer)
	return reconcile.Result{}, r.Update(ctx, machine)
}
