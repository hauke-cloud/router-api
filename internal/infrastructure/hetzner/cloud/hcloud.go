package cloud

import (
	"context"
	"fmt"
	"net"
	"net/netip"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

// Option configures the Hetzner Cloud client.
type Option func(*[]hcloud.ClientOption)

// WithEndpoint points the client at another API endpoint, for tests.
func WithEndpoint(endpoint string) Option {
	return func(opts *[]hcloud.ClientOption) {
		*opts = append(*opts, hcloud.WithEndpoint(endpoint))
	}
}

// WithApplication sets the name and version sent in the User-Agent header.
func WithApplication(name, version string) Option {
	return func(opts *[]hcloud.ClientOption) {
		*opts = append(*opts, hcloud.WithApplication(name, version))
	}
}

type hetzner struct {
	client *hcloud.Client
}

// NewHetzner returns a Cloud backed by the Hetzner Cloud API.
func NewHetzner(token string, options ...Option) Cloud {
	opts := []hcloud.ClientOption{hcloud.WithToken(token)}
	for _, option := range options {
		option(&opts)
	}
	return &hetzner{client: hcloud.NewClient(opts...)}
}

// NewFactory returns a Factory for the Hetzner Cloud API.
func NewFactory(options ...Option) Factory {
	return func(token string) Cloud { return NewHetzner(token, options...) }
}

func (h *hetzner) NetworkID(ctx context.Context, name string) (int64, error) {
	network, _, err := h.client.Network.GetByName(ctx, name)
	if err != nil {
		return 0, fmt.Errorf("get network %s: %w", name, err)
	}
	if network == nil {
		return 0, fmt.Errorf("network %s: %w", name, ErrNotFound)
	}
	return network.ID, nil
}

func (h *hetzner) EnsureFirewall(ctx context.Context, name string, labels map[string]string, rules []FirewallRule) (int64, error) {
	converted := make([]hcloud.FirewallRule, len(rules))
	for i := range rules {
		converted[i] = toHcloudRule(&rules[i])
	}

	firewall, _, err := h.client.Firewall.GetByName(ctx, name)
	if err != nil {
		return 0, fmt.Errorf("get firewall %s: %w", name, err)
	}
	if firewall == nil {
		result, _, err := h.client.Firewall.Create(ctx, hcloud.FirewallCreateOpts{Name: name, Labels: labels, Rules: converted})
		if err != nil {
			return 0, fmt.Errorf("create firewall %s: %w", name, err)
		}
		return result.Firewall.ID, nil
	}
	// The rules are replaced unconditionally. Comparing them first would
	// save a request a minute and add a place for the two to drift apart.
	if _, _, err := h.client.Firewall.SetRules(ctx, firewall, hcloud.FirewallSetRulesOpts{Rules: converted}); err != nil {
		return 0, fmt.Errorf("set rules of firewall %s: %w", name, err)
	}
	return firewall.ID, nil
}

func toHcloudRule(rule *FirewallRule) hcloud.FirewallRule {
	out := hcloud.FirewallRule{
		Direction: hcloud.FirewallRuleDirectionIn,
		Protocol:  hcloud.FirewallRuleProtocol(rule.Protocol),
	}
	if rule.Port != "" {
		out.Port = &rule.Port
	}
	if rule.Description != "" {
		out.Description = &rule.Description
	}
	for _, source := range rule.Sources {
		out.SourceIPs = append(out.SourceIPs, net.IPNet{
			IP:   source.Addr().AsSlice(),
			Mask: net.CIDRMask(source.Bits(), source.Addr().BitLen()),
		})
	}
	return out
}

func (h *hetzner) DeleteFirewall(ctx context.Context, name string) error {
	firewall, _, err := h.client.Firewall.GetByName(ctx, name)
	if err != nil {
		return fmt.Errorf("get firewall %s: %w", name, err)
	}
	if firewall == nil {
		return nil
	}
	if _, err := h.client.Firewall.Delete(ctx, firewall); err != nil {
		return deleteError("firewall "+name, err)
	}
	return nil
}

func (h *hetzner) EnsurePlacementGroup(ctx context.Context, name string, labels map[string]string) (int64, error) {
	group, _, err := h.client.PlacementGroup.GetByName(ctx, name)
	if err != nil {
		return 0, fmt.Errorf("get placement group %s: %w", name, err)
	}
	if group != nil {
		return group.ID, nil
	}
	result, _, err := h.client.PlacementGroup.Create(ctx, hcloud.PlacementGroupCreateOpts{
		Name: name, Labels: labels, Type: hcloud.PlacementGroupTypeSpread,
	})
	if err != nil {
		return 0, fmt.Errorf("create placement group %s: %w", name, err)
	}
	return result.PlacementGroup.ID, nil
}

func (h *hetzner) DeletePlacementGroup(ctx context.Context, name string) error {
	group, _, err := h.client.PlacementGroup.GetByName(ctx, name)
	if err != nil {
		return fmt.Errorf("get placement group %s: %w", name, err)
	}
	if group == nil {
		return nil
	}
	if _, err := h.client.PlacementGroup.Delete(ctx, group); err != nil {
		return deleteError("placement group "+name, err)
	}
	return nil
}

// deleteError maps what Hetzner answers to a delete onto the two cases the
// provider treats specially.
func deleteError(what string, err error) error {
	switch {
	case hcloud.IsError(err, hcloud.ErrorCodeNotFound):
		return nil
	case hcloud.IsError(err, hcloud.ErrorCodeResourceInUse, hcloud.ErrorCodeLocked, hcloud.ErrorCodeConflict):
		return fmt.Errorf("%s: %w: %w", what, ErrInUse, err)
	}
	return fmt.Errorf("delete %s: %w", what, err)
}

func (h *hetzner) PrimaryIPv4(ctx context.Context, name string) (*PrimaryIP, error) {
	primary, _, err := h.client.PrimaryIP.GetByName(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("get primary IP %s: %w", name, err)
	}
	if primary == nil || primary.Type != hcloud.PrimaryIPTypeIPv4 {
		return nil, fmt.Errorf("IPv4 primary IP %s: %w", name, ErrNotFound)
	}
	out := &PrimaryIP{ID: primary.ID, Name: primary.Name, AssigneeID: primary.AssigneeID, AutoDelete: primary.AutoDelete}
	if addr, ok := netip.AddrFromSlice(primary.IP); ok {
		out.IP = addr.Unmap()
	}
	if primary.Location != nil {
		out.Location = primary.Location.Name
	}
	return out, nil
}

func (h *hetzner) ServerByName(ctx context.Context, name string) (*Server, error) {
	server, _, err := h.client.Server.GetByName(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("get server %s: %w", name, err)
	}
	if server == nil {
		return nil, fmt.Errorf("server %s: %w", name, ErrNotFound)
	}
	return fromHcloudServer(server), nil
}

func fromHcloudServer(server *hcloud.Server) *Server {
	out := &Server{
		ID:     server.ID,
		Name:   server.Name,
		Status: string(server.Status),
		Labels: server.Labels,
	}
	if server.Location != nil {
		out.Location = server.Location.Name
	}
	if addr, ok := netip.AddrFromSlice(server.PublicNet.IPv4.IP); ok && !server.PublicNet.IPv4.IsUnspecified() {
		out.PublicIPv4 = addr.Unmap()
	}
	if addr, ok := netip.AddrFromSlice(server.PublicNet.IPv6.IP); ok && !server.PublicNet.IPv6.IsUnspecified() {
		// Hetzner reports the server's /64. Its images configure the first
		// address of it.
		out.PublicIPv6 = addr.Next()
	}
	for _, private := range server.PrivateNet {
		if addr, ok := netip.AddrFromSlice(private.IP); ok {
			out.PrivateIPs = append(out.PrivateIPs, addr.Unmap())
		}
	}
	return out
}

func (h *hetzner) CreateServer(ctx context.Context, spec *ServerSpec) (*Server, error) {
	serverType, _, err := h.client.ServerType.GetByName(ctx, spec.ServerType)
	if err != nil {
		return nil, fmt.Errorf("get server type %s: %w", spec.ServerType, err)
	}
	if serverType == nil {
		return nil, fmt.Errorf("server type %s does not exist", spec.ServerType)
	}
	// An image name is only unique per architecture.
	image, _, err := h.client.Image.GetForArchitecture(ctx, spec.Image, serverType.Architecture)
	if err != nil {
		return nil, fmt.Errorf("get image %s: %w", spec.Image, err)
	}
	if image == nil {
		return nil, fmt.Errorf("image %s does not exist for architecture %s", spec.Image, serverType.Architecture)
	}

	opts := hcloud.ServerCreateOpts{
		Name:             spec.Name,
		ServerType:       serverType,
		Image:            image,
		Location:         &hcloud.Location{Name: spec.Location},
		UserData:         spec.UserData,
		Labels:           spec.Labels,
		StartAfterCreate: hcloud.Ptr(true),
		PublicNet:        &hcloud.ServerCreatePublicNet{EnableIPv4: spec.EnableIPv4, EnableIPv6: spec.EnableIPv6},
	}
	for _, name := range spec.SSHKeys {
		key, _, err := h.client.SSHKey.GetByName(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("get SSH key %s: %w", name, err)
		}
		if key == nil {
			// Creating the server without it would produce one that nobody
			// can log in to for debugging.
			return nil, fmt.Errorf("SSH key %s does not exist", name)
		}
		opts.SSHKeys = append(opts.SSHKeys, key)
	}
	if spec.PrimaryIPv4ID != 0 {
		opts.PublicNet.EnableIPv4 = true
		opts.PublicNet.IPv4 = &hcloud.PrimaryIP{ID: spec.PrimaryIPv4ID}
	}
	if spec.NetworkID != 0 {
		opts.Networks = []*hcloud.Network{{ID: spec.NetworkID}}
	}
	if spec.FirewallID != 0 {
		opts.Firewalls = []*hcloud.ServerCreateFirewall{{Firewall: hcloud.Firewall{ID: spec.FirewallID}}}
	}
	if spec.PlacementGroupID != 0 {
		opts.PlacementGroup = &hcloud.PlacementGroup{ID: spec.PlacementGroupID}
	}

	result, _, err := h.client.Server.Create(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("create server %s: %w", spec.Name, err)
	}
	return fromHcloudServer(result.Server), nil
}

func (h *hetzner) DeleteServer(ctx context.Context, id int64) error {
	_, _, err := h.client.Server.DeleteWithResult(ctx, &hcloud.Server{ID: id})
	if err != nil && !hcloud.IsError(err, hcloud.ErrorCodeNotFound) {
		return fmt.Errorf("delete server %d: %w", id, err)
	}
	return nil
}

func (h *hetzner) ResetServer(ctx context.Context, id int64) error {
	if _, _, err := h.client.Server.Reset(ctx, &hcloud.Server{ID: id}); err != nil {
		return fmt.Errorf("reset server %d: %w", id, err)
	}
	return nil
}
