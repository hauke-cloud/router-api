// Package fake is an in-memory Hetzner Cloud project for tests.
package fake

import (
	"context"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"sync"

	"github.com/hauke-cloud/router-api/internal/infrastructure/hetzner/cloud"
)

// Firewall is a firewall in the fake project.
type Firewall struct {
	ID     int64
	Labels map[string]string
	Rules  []cloud.FirewallRule
}

// Cloud is a fake Hetzner Cloud project. The zero value is not usable; use
// New.
type Cloud struct {
	mu              sync.Mutex
	nextID          int64
	networks        map[string]int64
	firewalls       map[string]*Firewall
	placementGroups map[string]int64
	servers         map[int64]*cloud.Server
	specs           map[int64]cloud.ServerSpec
	resets          map[int64]int
	primaryIPs      map[string]*cloud.PrimaryIP
	// Tokens records every token a Cloud was requested for through Factory.
	Tokens []string
	// Err, if set, is returned by every call.
	Err error
	// OnCreate, if set, is called for every server that is created and may
	// change it, for instance to give it an address something listens on.
	OnCreate func(spec *cloud.ServerSpec, server *cloud.Server)
	// OnDelete and OnReset, if set, are called with the server's name.
	OnDelete func(name string)
	OnReset  func(name string)
}

// New returns an empty project with the given networks.
func New(networks ...string) *Cloud {
	c := &Cloud{
		nextID:          1000,
		networks:        map[string]int64{},
		firewalls:       map[string]*Firewall{},
		placementGroups: map[string]int64{},
		servers:         map[int64]*cloud.Server{},
		specs:           map[int64]cloud.ServerSpec{},
		resets:          map[int64]int{},
		primaryIPs:      map[string]*cloud.PrimaryIP{},
	}
	for _, name := range networks {
		c.networks[name] = c.id()
	}
	return c
}

// Factory returns a cloud.Factory that always hands out this project.
func (c *Cloud) Factory() cloud.Factory {
	return func(token string) cloud.Cloud {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.Tokens = append(c.Tokens, token)
		return c
	}
}

func (c *Cloud) id() int64 {
	c.nextID++
	return c.nextID
}

// Servers returns the servers in the project, by name.
func (c *Cloud) Servers() map[string]cloud.Server {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]cloud.Server{}
	for _, server := range c.servers {
		out[server.Name] = *server
	}
	return out
}

// Spec returns what the named server was created with.
func (c *Cloud) Spec(name string) (cloud.ServerSpec, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, server := range c.servers {
		if server.Name == name {
			return c.specs[id], true
		}
	}
	return cloud.ServerSpec{}, false
}

// SetServerStatus changes the status of the named server.
func (c *Cloud) SetServerStatus(name, status string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, server := range c.servers {
		if server.Name == name {
			server.Status = status
		}
	}
}

// RemoveServer makes the named server disappear, as if deleted by hand.
func (c *Cloud) RemoveServer(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	maps.DeleteFunc(c.servers, func(_ int64, server *cloud.Server) bool { return server.Name == name })
}

// AddServer puts a server nobody asked the provider for into the project.
func (c *Cloud) AddServer(name string, labels map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.id()
	c.servers[id] = &cloud.Server{ID: id, Name: name, Status: cloud.ServerStatusRunning, Labels: labels}
}

// Resets returns how often the named server was reset.
func (c *Cloud) Resets(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, server := range c.servers {
		if server.Name == name {
			return c.resets[id]
		}
	}
	return 0
}

// Firewall returns the named firewall, or nil.
func (c *Cloud) Firewall(name string) *Firewall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.firewalls[name]
}

// PlacementGroups returns the names of the placement groups.
func (c *Cloud) PlacementGroups() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Sorted(maps.Keys(c.placementGroups))
}

// AddPrimaryIP puts an unassigned Primary IP into the project.
func (c *Cloud) AddPrimaryIP(name, address string, autoDelete bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.primaryIPs[name] = &cloud.PrimaryIP{ID: c.id(), Name: name, IP: netip.MustParseAddr(address), AutoDelete: autoDelete, Location: "fsn1"}
}

// AssignPrimaryIP marks a Primary IP as assigned to some server.
func (c *Cloud) AssignPrimaryIP(name string, serverID int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.primaryIPs[name].AssigneeID = serverID
}

// PrimaryIPv4 implements cloud.Cloud.
func (c *Cloud) PrimaryIPv4(_ context.Context, name string) (*cloud.PrimaryIP, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Err != nil {
		return nil, c.Err
	}
	ip, ok := c.primaryIPs[name]
	if !ok {
		return nil, fmt.Errorf("primary IP %s: %w", name, cloud.ErrNotFound)
	}
	found := *ip
	return &found, nil
}

// NetworkID implements cloud.Cloud.
func (c *Cloud) NetworkID(_ context.Context, name string) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Err != nil {
		return 0, c.Err
	}
	id, ok := c.networks[name]
	if !ok {
		return 0, fmt.Errorf("network %s: %w", name, cloud.ErrNotFound)
	}
	return id, nil
}

// EnsureFirewall implements cloud.Cloud.
func (c *Cloud) EnsureFirewall(_ context.Context, name string, labels map[string]string, rules []cloud.FirewallRule) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Err != nil {
		return 0, c.Err
	}
	firewall, ok := c.firewalls[name]
	if !ok {
		firewall = &Firewall{ID: c.id(), Labels: labels}
		c.firewalls[name] = firewall
	}
	firewall.Rules = slices.Clone(rules)
	return firewall.ID, nil
}

// DeleteFirewall implements cloud.Cloud.
func (c *Cloud) DeleteFirewall(_ context.Context, name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Err != nil {
		return c.Err
	}
	firewall, ok := c.firewalls[name]
	if !ok {
		return nil
	}
	if c.inUse(func(spec *cloud.ServerSpec) bool { return spec.FirewallID == firewall.ID }) {
		return fmt.Errorf("firewall %s: %w", name, cloud.ErrInUse)
	}
	delete(c.firewalls, name)
	return nil
}

// EnsurePlacementGroup implements cloud.Cloud.
func (c *Cloud) EnsurePlacementGroup(_ context.Context, name string, _ map[string]string) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Err != nil {
		return 0, c.Err
	}
	if id, ok := c.placementGroups[name]; ok {
		return id, nil
	}
	id := c.id()
	c.placementGroups[name] = id
	return id, nil
}

// DeletePlacementGroup implements cloud.Cloud.
func (c *Cloud) DeletePlacementGroup(_ context.Context, name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Err != nil {
		return c.Err
	}
	id, ok := c.placementGroups[name]
	if !ok {
		return nil
	}
	if c.inUse(func(spec *cloud.ServerSpec) bool { return spec.PlacementGroupID == id }) {
		return fmt.Errorf("placement group %s: %w", name, cloud.ErrInUse)
	}
	delete(c.placementGroups, name)
	return nil
}

// inUse reports whether an existing server was created with a spec that
// matches.
func (c *Cloud) inUse(matches func(*cloud.ServerSpec) bool) bool {
	for id := range c.servers {
		if spec := c.specs[id]; matches(&spec) {
			return true
		}
	}
	return false
}

// ServerByName implements cloud.Cloud.
func (c *Cloud) ServerByName(_ context.Context, name string) (*cloud.Server, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Err != nil {
		return nil, c.Err
	}
	for _, server := range c.servers {
		if server.Name == name {
			found := *server
			return &found, nil
		}
	}
	return nil, fmt.Errorf("server %s: %w", name, cloud.ErrNotFound)
}

// CreateServer implements cloud.Cloud. The server starts out initializing,
// as a real one does.
func (c *Cloud) CreateServer(_ context.Context, spec *cloud.ServerSpec) (*cloud.Server, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Err != nil {
		return nil, c.Err
	}
	for _, server := range c.servers {
		if server.Name == spec.Name {
			return nil, fmt.Errorf("server name %s is already used", spec.Name)
		}
	}
	id := c.id()
	server := &cloud.Server{
		ID:       id,
		Name:     spec.Name,
		Status:   cloud.ServerStatusInitializing,
		Location: spec.Location,
		Labels:   maps.Clone(spec.Labels),
	}
	n := byte(id % 250) //nolint:gosec // test addresses
	if spec.EnableIPv4 {
		server.PublicIPv4 = netip.AddrFrom4([4]byte{203, 0, 113, n})
	}
	if spec.PrimaryIPv4ID != 0 {
		var primary *cloud.PrimaryIP
		for _, ip := range c.primaryIPs {
			if ip.ID == spec.PrimaryIPv4ID {
				primary = ip
			}
		}
		switch {
		case primary == nil:
			return nil, fmt.Errorf("primary IP %d: %w", spec.PrimaryIPv4ID, cloud.ErrNotFound)
		case primary.AssigneeID != 0:
			return nil, fmt.Errorf("primary IP %s: %w", primary.Name, cloud.ErrInUse)
		}
		primary.AssigneeID = id
		server.PublicIPv4 = primary.IP
	}
	if spec.EnableIPv6 {
		server.PublicIPv6 = netip.MustParseAddr(fmt.Sprintf("2001:db8:%x::1", id))
	}
	if spec.NetworkID != 0 {
		server.PrivateIPs = []netip.Addr{netip.AddrFrom4([4]byte{10, 0, 1, n})}
	}
	if c.OnCreate != nil {
		c.OnCreate(spec, server)
	}
	c.servers[id] = server
	c.specs[id] = *spec
	created := *server
	return &created, nil
}

// DeleteServer implements cloud.Cloud.
func (c *Cloud) DeleteServer(_ context.Context, id int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Err != nil {
		return c.Err
	}
	if server, ok := c.servers[id]; ok && c.OnDelete != nil {
		c.OnDelete(server.Name)
	}
	// As at Hetzner: the address is detached, or goes with the server.
	for name, ip := range c.primaryIPs {
		if ip.AssigneeID != id {
			continue
		}
		if ip.AutoDelete {
			delete(c.primaryIPs, name)
		} else {
			ip.AssigneeID = 0
		}
	}
	delete(c.servers, id)
	return nil
}

// ResetServer implements cloud.Cloud.
func (c *Cloud) ResetServer(_ context.Context, id int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Err != nil {
		return c.Err
	}
	server, ok := c.servers[id]
	if !ok {
		return fmt.Errorf("server %d: %w", id, cloud.ErrNotFound)
	}
	c.resets[id]++
	if c.OnReset != nil {
		c.OnReset(server.Name)
	}
	return nil
}
