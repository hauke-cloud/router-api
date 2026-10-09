package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

// api is a scripted Hetzner Cloud API: responses by "METHOD path", and a
// record of what was sent.
type api struct {
	t         *testing.T
	responses map[string]string
	status    map[string]int
	bodies    map[string]map[string]any
	calls     []string
}

func newAPI(t *testing.T, responses map[string]string) (*api, Cloud) {
	t.Helper()
	a := &api{t: t, responses: responses, status: map[string]int{}, bodies: map[string]map[string]any{}}
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	return a, NewHetzner("token", WithEndpoint(srv.URL))
}

func (a *api) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := r.Method + " " + r.URL.Path
	if r.URL.RawQuery != "" && r.Method == http.MethodGet {
		// Lookups by name are queries; keep the name in the key.
		if name := r.URL.Query().Get("name"); name != "" {
			key += "?name=" + name
		}
	}
	a.calls = append(a.calls, key)
	if r.Method != http.MethodGet && r.Method != http.MethodDelete {
		raw, _ := io.ReadAll(r.Body)
		// Actions such as a reset are sent without a body.
		var body map[string]any
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &body); err != nil {
				a.t.Errorf("%s: body %q: %v", key, raw, err)
			}
		}
		a.bodies[key] = body
	}
	response, ok := a.responses[key]
	if !ok {
		a.t.Errorf("unexpected request %s", key)
		http.Error(w, `{"error":{"code":"not_found","message":"not scripted"}}`, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	status := a.status[key]
	if status == 0 {
		status = http.StatusOK
		if r.Method == http.MethodPost {
			status = http.StatusCreated
		}
	}
	w.WriteHeader(status)
	_, _ = io.WriteString(w, response)
}

const serverJSON = `{"id":4711,"name":"edge-abc","status":"initializing","labels":{"a":"b"},
  "location":{"id":1,"name":"fsn1"},
  "public_net":{"ipv4":{"id":1,"ip":"203.0.113.7","blocked":false},"ipv6":{"id":2,"ip":"2001:db8:1::/64","blocked":false},"floating_ips":[],"firewalls":[]},
  "private_net":[{"network":7,"ip":"10.0.1.2","alias_ips":[],"mac_address":"86:00:00:00:00:01"}]}`

func TestNetworkID(t *testing.T) {
	_, c := newAPI(t, map[string]string{
		"GET /networks?name=lab":  `{"networks":[{"id":7,"name":"lab","ip_range":"10.0.0.0/16"}]}`,
		"GET /networks?name=nope": `{"networks":[]}`,
	})
	id, err := c.NetworkID(context.Background(), "lab")
	if err != nil || id != 7 {
		t.Errorf("NetworkID = %d, %v", id, err)
	}
	if _, err := c.NetworkID(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestServerByName(t *testing.T) {
	_, c := newAPI(t, map[string]string{
		"GET /servers?name=edge-abc": `{"servers":[` + serverJSON + `]}`,
		"GET /servers?name=nope":     `{"servers":[]}`,
	})
	server, err := c.ServerByName(context.Background(), "edge-abc")
	if err != nil {
		t.Fatalf("ServerByName: %v", err)
	}
	if server.ID != 4711 || server.Status != ServerStatusInitializing || server.Location != "fsn1" || server.Labels["a"] != "b" {
		t.Errorf("server = %+v", server)
	}
	if server.PublicIPv4 != netip.MustParseAddr("203.0.113.7") {
		t.Errorf("ipv4 = %v", server.PublicIPv4)
	}
	// Hetzner reports the /64; the server's own address in it is ::1.
	if server.PublicIPv6 != netip.MustParseAddr("2001:db8:1::1") {
		t.Errorf("ipv6 = %v", server.PublicIPv6)
	}
	if len(server.PrivateIPs) != 1 || server.PrivateIPs[0] != netip.MustParseAddr("10.0.1.2") {
		t.Errorf("private = %v", server.PrivateIPs)
	}
	if _, err := c.ServerByName(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestCreateServer(t *testing.T) {
	a, c := newAPI(t, map[string]string{
		"GET /server_types?name=cx23":   `{"server_types":[{"id":3,"name":"cx23","architecture":"x86"}]}`,
		"GET /images?name=ubuntu-24.04": `{"images":[{"id":99,"name":"ubuntu-24.04","type":"system","architecture":"x86"}]}`,
		"GET /ssh_keys?name=admin":      `{"ssh_keys":[{"id":5,"name":"admin"}]}`,
		"POST /servers":                 `{"server":` + serverJSON + `,"action":{"id":1,"status":"running"},"next_actions":[],"root_password":null}`,
	})

	server, err := c.CreateServer(context.Background(), &ServerSpec{
		Name: "edge-abc", ServerType: "cx23", Location: "fsn1", Image: "ubuntu-24.04",
		UserData: "#cloud-config\n", Labels: map[string]string{"a": "b"}, SSHKeys: []string{"admin"},
		NetworkID: 7, FirewallID: 8, PlacementGroupID: 9, EnableIPv4: true, EnableIPv6: false,
	})
	if err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	if server.ID != 4711 {
		t.Errorf("server = %+v", server)
	}

	body := a.bodies["POST /servers"]
	want := map[string]any{
		"name": "edge-abc", "location": "fsn1", "user_data": "#cloud-config\n",
		"start_after_create": true, "placement_group": float64(9),
	}
	for key, value := range want {
		if body[key] != value {
			t.Errorf("%s = %v, want %v", key, body[key], value)
		}
	}
	if networks, _ := body["networks"].([]any); len(networks) != 1 || networks[0] != float64(7) {
		t.Errorf("networks = %v", body["networks"])
	}
	if firewalls, _ := json.Marshal(body["firewalls"]); string(firewalls) != `[{"firewall":8}]` {
		t.Errorf("firewalls = %s", firewalls)
	}
	if keys, _ := body["ssh_keys"].([]any); len(keys) != 1 {
		t.Errorf("ssh_keys = %v", body["ssh_keys"])
	}
	publicNet, _ := body["public_net"].(map[string]any)
	if publicNet["enable_ipv4"] != true || publicNet["enable_ipv6"] != false {
		t.Errorf("public_net = %v", publicNet)
	}
}

func TestCreateServerUnknownSSHKey(t *testing.T) {
	_, c := newAPI(t, map[string]string{
		"GET /server_types?name=cx23":   `{"server_types":[{"id":3,"name":"cx23","architecture":"x86"}]}`,
		"GET /images?name=ubuntu-24.04": `{"images":[{"id":99,"name":"ubuntu-24.04","type":"system","architecture":"x86"}]}`,
		"GET /ssh_keys?name=typo":       `{"ssh_keys":[]}`,
	})
	_, err := c.CreateServer(context.Background(), &ServerSpec{
		Name: "edge-abc", ServerType: "cx23", Location: "fsn1", Image: "ubuntu-24.04", SSHKeys: []string{"typo"},
	})
	// A server created without the key would be one nobody can log in to.
	if err == nil || !strings.Contains(err.Error(), "typo") {
		t.Errorf("err = %v, want it to name the missing key", err)
	}
}

func TestEnsureFirewall(t *testing.T) {
	rules := []FirewallRule{
		{Description: "mgmt", Protocol: "tcp", Port: "443", Sources: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}},
		{Protocol: "icmp", Sources: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}},
	}

	t.Run("creates", func(t *testing.T) {
		a, c := newAPI(t, map[string]string{
			"GET /firewalls?name=fw": `{"firewalls":[]}`,
			"POST /firewalls":        `{"firewall":{"id":8,"name":"fw","rules":[],"applied_to":[]},"actions":[]}`,
		})
		id, err := c.EnsureFirewall(context.Background(), "fw", map[string]string{"a": "b"}, rules)
		if err != nil || id != 8 {
			t.Fatalf("EnsureFirewall = %d, %v", id, err)
		}
		sent, _ := json.Marshal(a.bodies["POST /firewalls"]["rules"])
		for _, want := range []string{`"direction":"in"`, `"port":"443"`, `"protocol":"tcp"`, `"source_ips":["192.0.2.0/24"]`, `"protocol":"icmp"`} {
			if !strings.Contains(string(sent), want) {
				t.Errorf("rules %s do not contain %s", sent, want)
			}
		}
	})

	t.Run("updates", func(t *testing.T) {
		a, c := newAPI(t, map[string]string{
			"GET /firewalls?name=fw":              `{"firewalls":[{"id":8,"name":"fw","rules":[],"applied_to":[]}]}`,
			"POST /firewalls/8/actions/set_rules": `{"actions":[]}`,
		})
		id, err := c.EnsureFirewall(context.Background(), "fw", nil, rules)
		if err != nil || id != 8 {
			t.Fatalf("EnsureFirewall = %d, %v", id, err)
		}
		if _, ok := a.bodies["POST /firewalls/8/actions/set_rules"]; !ok {
			t.Errorf("calls = %v, want the rules of the existing firewall replaced", a.calls)
		}
	})
}

func TestDeleteFirewall(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		_, c := newAPI(t, map[string]string{"GET /firewalls?name=fw": `{"firewalls":[]}`})
		if err := c.DeleteFirewall(context.Background(), "fw"); err != nil {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("in use", func(t *testing.T) {
		a, c := newAPI(t, map[string]string{
			"GET /firewalls?name=fw": `{"firewalls":[{"id":8,"name":"fw","rules":[],"applied_to":[]}]}`,
			"DELETE /firewalls/8":    `{"error":{"code":"resource_in_use","message":"firewall is in use"}}`,
		})
		a.status["DELETE /firewalls/8"] = http.StatusUnprocessableEntity
		if err := c.DeleteFirewall(context.Background(), "fw"); !errors.Is(err, ErrInUse) {
			t.Errorf("err = %v, want ErrInUse", err)
		}
	})
}

func TestDeleteServerThatIsGone(t *testing.T) {
	a, c := newAPI(t, map[string]string{
		"DELETE /servers/4711": `{"error":{"code":"not_found","message":"server not found"}}`,
	})
	a.status["DELETE /servers/4711"] = http.StatusNotFound
	if err := c.DeleteServer(context.Background(), 4711); err != nil {
		t.Errorf("err = %v, want nil for a server that is already gone", err)
	}
}

func TestResetServer(t *testing.T) {
	a, c := newAPI(t, map[string]string{
		"POST /servers/4711/actions/reset": `{"action":{"id":1,"status":"running","command":"reset_server"}}`,
	})
	if err := c.ResetServer(context.Background(), 4711); err != nil {
		t.Fatalf("ResetServer: %v", err)
	}
	if len(a.calls) != 1 {
		t.Errorf("calls = %v", a.calls)
	}
}

func TestPrimaryIPv4(t *testing.T) {
	_, c := newAPI(t, map[string]string{
		"GET /primary_ips?name=edge-0": `{"primary_ips":[{"id":42,"name":"edge-0","ip":"198.51.100.10","type":"ipv4",
		  "assignee_id":4711,"assignee_type":"server","auto_delete":false,"location":{"id":1,"name":"fsn1"}}]}`,
		"GET /primary_ips?name=v6":   `{"primary_ips":[{"id":43,"name":"v6","ip":"2001:db8::/64","type":"ipv6","auto_delete":true}]}`,
		"GET /primary_ips?name=nope": `{"primary_ips":[]}`,
	})
	primary, err := c.PrimaryIPv4(context.Background(), "edge-0")
	if err != nil {
		t.Fatalf("PrimaryIPv4: %v", err)
	}
	want := PrimaryIP{ID: 42, Name: "edge-0", IP: netip.MustParseAddr("198.51.100.10"), AssigneeID: 4711, Location: "fsn1"}
	if *primary != want {
		t.Errorf("primary IP = %+v, want %+v", *primary, want)
	}
	for _, name := range []string{"nope", "v6"} {
		if _, err := c.PrimaryIPv4(context.Background(), name); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: err = %v, want ErrNotFound", name, err)
		}
	}
}

func TestCreateServerWithAPrimaryIP(t *testing.T) {
	a, c := newAPI(t, map[string]string{
		"GET /server_types?name=cx23":   `{"server_types":[{"id":3,"name":"cx23","architecture":"x86"}]}`,
		"GET /images?name=ubuntu-24.04": `{"images":[{"id":99,"name":"ubuntu-24.04","type":"system","architecture":"x86"}]}`,
		"POST /servers":                 `{"server":` + serverJSON + `,"action":{"id":1,"status":"running"},"next_actions":[],"root_password":null}`,
	})
	if _, err := c.CreateServer(context.Background(), &ServerSpec{
		Name: "edge-abc", ServerType: "cx23", Location: "fsn1", Image: "ubuntu-24.04", PrimaryIPv4ID: 42,
	}); err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	publicNet, _ := a.bodies["POST /servers"]["public_net"].(map[string]any)
	if publicNet["ipv4"] != float64(42) || publicNet["enable_ipv4"] != true {
		t.Errorf("public_net = %v, want the Primary IP's ID", publicNet)
	}
}
