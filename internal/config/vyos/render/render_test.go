package render

import (
	"slices"
	"strings"
	"testing"

	corev1alpha1 "github.com/hauke-cloud/router-api/api/core/v1alpha1"
	"github.com/hauke-cloud/router-api/internal/config/vyos/command"
)

func testData() *Data {
	return &Data{
		Values: map[string]string{"wgKey": "kKMnp6QBm1Wj0zxnc1uBbSs3hhdpVIFeQy9hxRTBxWQ=", "banner": "it's edge", "empty": ""},
		Router: Router{Name: "edge-abc", Namespace: "routers", Group: "edge"},
		Machine: Machine{
			ExternalIP: "203.0.113.7",
			InternalIP: "10.0.1.2",
		},
		Peers: []Peer{{Name: "edge-def", InternalIP: "10.0.1.3", ExternalIP: "203.0.113.8"}},
		Host:  Host{PublicInterface: "eth0", PrivateInterface: "eth1"},
	}
}

func TestCommands(t *testing.T) {
	template := `
# identity
set system host-name {{ .Router.Name }}
set interfaces wireguard wg0 private-key {{ .Values.wgKey }}
set system login banner pre-login {{ quote .Values.banner }}
{{- range .Peers }}
set high-availability vrrp group wan peer-address {{ .InternalIP }}
{{- end }}
set high-availability vrrp group wan hello-source-address {{ .Machine.InternalIP }}
set high-availability vrrp group wan interface {{ .Host.PrivateInterface }}
`
	paths, err := Commands(template, testData())
	if err != nil {
		t.Fatalf("Commands: %v", err)
	}
	want := []string{
		"set system host-name edge-abc",
		"set interfaces wireguard wg0 private-key kKMnp6QBm1Wj0zxnc1uBbSs3hhdpVIFeQy9hxRTBxWQ=",
		"set system login banner pre-login 'it'\\''s edge'",
		"set high-availability vrrp group wan peer-address 10.0.1.3",
		"set high-availability vrrp group wan hello-source-address 10.0.1.2",
		"set high-availability vrrp group wan interface eth1",
	}
	if got := command.Lines(paths); !slices.Equal(got, want) {
		t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestCommandsErrors(t *testing.T) {
	tests := map[string]struct {
		template string
		want     string
	}{
		// A typo in a value name must not render as "<no value>" and end up
		// as a host name.
		"unknown value":  {"set system host-name {{ .Values.nope }}", "nope"},
		"unknown field":  {"set system host-name {{ .Router.Nope }}", "Nope"},
		"syntax":         {"set system host-name {{ .Router.Name", "template"},
		"not a command":  {"system host-name edge", "line 1"},
		"required unset": {`set system host-name {{ required "need a name" "" }}`, "need a name"},
		"delete":         {"delete system", "set"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Commands(tt.template, testData())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

func TestErrorsDoNotLeakValues(t *testing.T) {
	// The rendered text holds secrets. An error about line N must not quote
	// the line.
	data := testData()
	_, err := Commands("bogus {{ .Values.wgKey }}", data)
	if err == nil {
		t.Fatal("no error")
	}
	if strings.Contains(err.Error(), data.Values["wgKey"]) {
		t.Errorf("error quotes a value: %v", err)
	}
}

func TestText(t *testing.T) {
	out, err := Text("failover.json", `{"gateway":"{{ .Machine.InternalIP }}","token":"{{ default "none" .Values.empty }}"}`, testData())
	if err != nil {
		t.Fatalf("Text: %v", err)
	}
	if out != `{"gateway":"10.0.1.2","token":"none"}` {
		t.Errorf("out = %s", out)
	}
}

func TestPeersAreSorted(t *testing.T) {
	// The order of peers decides the order of rendered commands. It has to
	// be the same on every render or the configuration looks changed.
	peers := SortPeers([]Peer{{Name: "c"}, {Name: "a"}, {Name: "b"}})
	if peers[0].Name != "a" || peers[1].Name != "b" || peers[2].Name != "c" {
		t.Errorf("peers = %v", peers)
	}
}

func TestExpose(t *testing.T) {
	port := func(protocol corev1alpha1.ExposedProtocol, number int32) corev1alpha1.ExposedPort {
		return corev1alpha1.ExposedPort{Protocol: protocol, Port: number}
	}
	got := Expose([]corev1alpha1.ExposedEndpoint{
		{Address: "2001:db8::18", Ports: []corev1alpha1.ExposedPort{port("tcp", 443)}},
		{Address: "203.0.113.18", Ports: []corev1alpha1.ExposedPort{
			port("udp", 53), port("tcp", 8001), port("tcp", 443), port("tcp", 8000), port("tcp", 8002), port("tcp", 80),
		}},
		{Address: "not an address", Ports: []corev1alpha1.ExposedPort{port("tcp", 22)}},
	})
	want := []Exposed{
		{Address: "203.0.113.18", Family: "ipv4", Protocol: "tcp", Ports: "80,443,8000-8002"},
		{Address: "203.0.113.18", Family: "ipv4", Protocol: "udp", Ports: "53"},
		{Address: "2001:db8::18", Family: "ipv6", Protocol: "tcp", Ports: "443"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("Expose = %+v, want %+v", got, want)
	}
}
