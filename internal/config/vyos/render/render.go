// Package render turns the templates of a VyOSConfig into configuration.
package render

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"text/template"

	corev1alpha1 "github.com/hauke-cloud/router-api/api/core/v1alpha1"
	"github.com/hauke-cloud/router-api/internal/config/vyos/command"
)

// Data is what a template is rendered with. docs/configuration.md is its
// user-facing description; keep the two in step.
type Data struct {
	// Values from the VyOSConfig's spec.values, by name.
	Values map[string]string
	// Router this configuration is for.
	Router Router
	// Machine the router runs on.
	Machine Machine
	// Peers are the other routers of the same group, sorted by name.
	Peers []Peer
	// Host the VyOS container runs on.
	Host Host
	// Exposed is what the RouterExposure the configuration refers to lists,
	// one entry per address and protocol, sorted by both.
	Exposed []Exposed
}

// Exposed is one address and the ports of one protocol to let through to it.
type Exposed struct {
	Address string
	// Family is "ipv4" or "ipv6", the word VyOS uses for it.
	Family string
	// Protocol is "tcp" or "udp".
	Protocol string
	// Ports as VyOS takes them in one value: "80,443,8000-8010".
	Ports string
}

// Router identifies the router.
type Router struct {
	Name      string
	Namespace string
	// Group is the name of the RouterDeployment the router belongs to, empty
	// for a router created by hand.
	Group string
	// Slot of the router in its group, 0 for a router without one.
	Slot int
}

// Machine is the instance's addresses.
type Machine struct {
	// ExternalIP is the first public IPv4 address, if any.
	ExternalIP string
	// ExternalIPv6 is the first public IPv6 address, if any.
	ExternalIPv6 string
	// InternalIP is the first private address, if any.
	InternalIP string
}

// Peer is another router of the same group.
type Peer struct {
	Name string
	// Slot of the peer, 0 for a router without one.
	Slot       int
	ExternalIP string
	InternalIP string
}

// Host names the interfaces of the host.
type Host struct {
	PublicInterface  string
	PrivateInterface string
}

// Expose turns the endpoints of a RouterExposure into template data. Runs of
// consecutive ports become ranges.
func Expose(endpoints []corev1alpha1.ExposedEndpoint) []Exposed {
	var exposed []Exposed
	for i := range endpoints {
		endpoint := &endpoints[i]
		address, err := netip.ParseAddr(endpoint.Address)
		if err != nil {
			continue
		}
		family := "ipv4"
		if address.Is6() {
			family = "ipv6"
		}
		for _, protocol := range []corev1alpha1.ExposedProtocol{corev1alpha1.ExposedTCP, corev1alpha1.ExposedUDP} {
			var numbers []int
			for _, port := range endpoint.Ports {
				if port.Protocol == protocol {
					numbers = append(numbers, int(port.Port))
				}
			}
			if len(numbers) == 0 {
				continue
			}
			slices.Sort(numbers)
			numbers = slices.Compact(numbers)
			var ports []string
			for start := 0; start < len(numbers); {
				end := start
				for end+1 < len(numbers) && numbers[end+1] == numbers[end]+1 {
					end++
				}
				if end > start {
					ports = append(ports, strconv.Itoa(numbers[start])+"-"+strconv.Itoa(numbers[end]))
				} else {
					ports = append(ports, strconv.Itoa(numbers[start]))
				}
				start = end + 1
			}
			exposed = append(exposed, Exposed{
				Address: address.String(), Family: family, Protocol: string(protocol), Ports: strings.Join(ports, ","),
			})
		}
	}
	slices.SortFunc(exposed, func(a, b Exposed) int {
		return cmp.Or(netip.MustParseAddr(a.Address).Compare(netip.MustParseAddr(b.Address)), strings.Compare(a.Protocol, b.Protocol))
	})
	return exposed
}

// SortPeers orders peers by name.
func SortPeers(peers []Peer) []Peer {
	slices.SortFunc(peers, func(a, b Peer) int { return strings.Compare(a.Name, b.Name) })
	return peers
}

var funcs = template.FuncMap{
	// quote makes a value safe to use as one token of a command.
	"quote": func(value string) string {
		return command.Path{value}.String()[len("set "):]
	},
	// default returns fallback when value is empty.
	"default": func(fallback, value string) string {
		if value == "" {
			return fallback
		}
		return value
	},
	// required fails the render with message when value is empty.
	"required": func(message, value string) (string, error) {
		if value == "" {
			return "", errors.New(message)
		}
		return value, nil
	},
	// add sums integers, for rule numbers counted up from a base.
	"add": func(numbers ...int) int {
		sum := 0
		for _, number := range numbers {
			sum += number
		}
		return sum
	},
	"join":  func(separator string, items []string) string { return strings.Join(items, separator) },
	"lower": strings.ToLower,
	"upper": strings.ToUpper,
}

// Text renders one template. name only appears in error messages.
func Text(name, text string, data *Data) (string, error) {
	// missingkey=error: a misspelled value is an error, not "<no value>".
	tmpl, err := template.New(name).Funcs(funcs).Option("missingkey=error").Parse(text)
	if err != nil {
		return "", fmt.Errorf("parse template: %w", err)
	}
	var out bytes.Buffer
	if err := tmpl.Execute(&out, data); err != nil {
		return "", fmt.Errorf("render template: %w", err)
	}
	return out.String(), nil
}

// Commands renders a configuration template and parses the result.
func Commands(text string, data *Data) ([]command.Path, error) {
	rendered, err := Text("commands", text, data)
	if err != nil {
		return nil, err
	}
	paths, err := command.Parse(rendered)
	if err != nil {
		// command.Parse names the line and the problem, never the content.
		return nil, fmt.Errorf("rendered configuration: %w", err)
	}
	return paths, nil
}
