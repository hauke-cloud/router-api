// Package cloud is the part of Hetzner Cloud the infrastructure provider
// uses, behind an interface small enough to fake.
package cloud

import (
	"context"
	"errors"
	"net/netip"
)

// ErrNotFound is returned when the named resource does not exist.
var ErrNotFound = errors.New("not found")

// ErrInUse is returned when a resource cannot be deleted because something
// still uses it.
var ErrInUse = errors.New("still in use")

// Server statuses the provider distinguishes. Hetzner has more; everything
// else is "not running".
const (
	ServerStatusRunning      = "running"
	ServerStatusInitializing = "initializing"
	ServerStatusStarting     = "starting"
	ServerStatusOff          = "off"
)

// Server is a Hetzner Cloud server.
type Server struct {
	ID       int64
	Name     string
	Status   string
	Location string
	Labels   map[string]string
	// PublicIPv4 and PublicIPv6 are invalid (zero) if the server has none.
	// PublicIPv6 is the first address of the server's /64.
	PublicIPv4 netip.Addr
	PublicIPv6 netip.Addr
	// PrivateIPs are the server's addresses in the networks it is attached
	// to.
	PrivateIPs []netip.Addr
}

// ServerSpec describes a server to create.
type ServerSpec struct {
	Name       string
	ServerType string
	Location   string
	// Image is the name of a system image or the ID of a snapshot.
	Image    string
	UserData string
	Labels   map[string]string
	SSHKeys  []string
	// NetworkID, FirewallID and PlacementGroupID attach the server; 0 means
	// none.
	NetworkID        int64
	FirewallID       int64
	PlacementGroupID int64
	EnableIPv4       bool
	EnableIPv6       bool
}

// FirewallRule allows inbound traffic.
type FirewallRule struct {
	Description string
	// Protocol is tcp, udp, icmp, esp or gre.
	Protocol string
	// Port is a port or a range; empty for protocols without ports.
	Port    string
	Sources []netip.Prefix
}

// Cloud is the Hetzner Cloud API as the provider needs it. Calls that change
// something return once Hetzner has accepted the change, not once it is in
// effect: a created server is typically still initializing.
type Cloud interface {
	// NetworkID returns the ID of the named network, or ErrNotFound.
	NetworkID(ctx context.Context, name string) (int64, error)

	// EnsureFirewall creates the named firewall or replaces its rules, and
	// returns its ID.
	EnsureFirewall(ctx context.Context, name string, labels map[string]string, rules []FirewallRule) (int64, error)
	// DeleteFirewall deletes the named firewall. A firewall that does not
	// exist is not an error; one that is still attached is ErrInUse.
	DeleteFirewall(ctx context.Context, name string) error

	// EnsurePlacementGroup creates the named spread placement group if it is
	// missing and returns its ID.
	EnsurePlacementGroup(ctx context.Context, name string, labels map[string]string) (int64, error)
	// DeletePlacementGroup deletes the named placement group, with the same
	// rules as DeleteFirewall.
	DeletePlacementGroup(ctx context.Context, name string) error

	// ServerByName returns the named server, or ErrNotFound.
	ServerByName(ctx context.Context, name string) (*Server, error)
	// CreateServer creates and starts a server.
	CreateServer(ctx context.Context, spec *ServerSpec) (*Server, error)
	// DeleteServer deletes a server. One that does not exist is not an error.
	DeleteServer(ctx context.Context, id int64) error
	// ResetServer power-cycles a server.
	ResetServer(ctx context.Context, id int64) error
}

// Factory returns a Cloud for an API token.
type Factory func(token string) Cloud
