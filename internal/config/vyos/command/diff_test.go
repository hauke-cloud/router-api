package command

import (
	"slices"
	"testing"
)

func mustParse(t *testing.T, text string) []Path {
	t.Helper()
	paths, err := Parse(text)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return paths
}

func TestDiff(t *testing.T) {
	tests := map[string]struct {
		current, desired string
		keep             []Path
		wantDelete       []string
		wantSet          []string
	}{
		"nothing to do": {
			current: "set system host-name edge\n",
			desired: "set system host-name edge\n",
		},
		"add": {
			current: "set system host-name edge\n",
			desired: "set system host-name edge\nset system time-zone UTC\n",
			wantSet: []string{"set system time-zone UTC"},
		},
		"change a value": {
			current:    "set system host-name old\n",
			desired:    "set system host-name new\n",
			wantDelete: []string{"delete system host-name old"},
			wantSet:    []string{"set system host-name new"},
		},
		"remove one value of several": {
			current:    "set system name-server 1.1.1.1\nset system name-server 8.8.8.8\n",
			desired:    "set system name-server 1.1.1.1\n",
			wantDelete: []string{"delete system name-server 8.8.8.8"},
		},
		// Deleting leaf by leaf would leave "interfaces dummy dum0" behind
		// as an empty node, to be found and deleted by the next run.
		"remove a whole subtree at its root": {
			current: `set interfaces dummy dum0 address 192.0.2.1/32
set interfaces dummy dum0 description x
set interfaces loopback lo
`,
			desired:    "set interfaces loopback lo\n",
			wantDelete: []string{"delete interfaces dummy"},
		},
		"remove a top level section": {
			current:    "set nat source rule 100 translation address masquerade\nset system host-name edge\n",
			desired:    "set system host-name edge\n",
			wantDelete: []string{"delete nat"},
		},
		"keep a sibling": {
			current: `set firewall ipv4 input filter rule 10 action accept
set firewall ipv4 input filter rule 20 action drop
`,
			desired:    "set firewall ipv4 input filter rule 10 action accept\n",
			wantDelete: []string{"delete firewall ipv4 input filter rule 20"},
		},
		"kept paths are never deleted": {
			current: `set system login user vyos authentication encrypted-password x
set service ntp server time1.vyos.net
set system host-name edge
`,
			desired:    "set system host-name edge\n",
			keep:       []Path{{"system", "login"}},
			wantDelete: []string{"delete service"},
		},
		"kept paths are still set": {
			current: "set system login user vyos authentication encrypted-password x\n",
			desired: "set system login user admin authentication encrypted-password y\n",
			keep:    []Path{{"system", "login"}},
			wantSet: []string{"set system login user admin authentication encrypted-password y"},
		},
		// VyOS writes the MAC address of every interface into its own
		// configuration. Nobody lists those, and they must stay.
		"kept paths may contain wildcards": {
			current: `set interfaces ethernet eth0 hw-id 96:00:00:00:00:01
set interfaces ethernet eth0 address dhcp
set interfaces ethernet eth1 hw-id 96:00:00:00:00:02
set interfaces ethernet eth1 description old
`,
			desired:    "set interfaces ethernet eth0 address dhcp\n",
			keep:       []Path{{"interfaces", "ethernet", Wildcard, "hw-id"}},
			wantDelete: []string{"delete interfaces ethernet eth1 description"},
		},
		"a valueless node that gains children is not deleted": {
			current: "set interfaces loopback lo\n",
			desired: "set interfaces loopback lo address 192.0.2.1/32\n",
			wantSet: []string{"set interfaces loopback lo address 192.0.2.1/32"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			changes := Diff(mustParse(t, tt.current), mustParse(t, tt.desired), tt.keep)

			var gotDelete, gotSet []string
			for _, op := range changes {
				switch op.Op {
				case OpDelete:
					if len(gotSet) > 0 {
						t.Error("a delete follows a set: deletes have to come first, or a changed value deletes itself")
					}
					gotDelete = append(gotDelete, op.String())
				case OpSet:
					gotSet = append(gotSet, op.String())
				}
			}
			if !slices.Equal(gotDelete, tt.wantDelete) {
				t.Errorf("deletes = %q, want %q", gotDelete, tt.wantDelete)
			}
			if !slices.Equal(gotSet, tt.wantSet) {
				t.Errorf("sets = %q, want %q", gotSet, tt.wantSet)
			}
		})
	}
}
