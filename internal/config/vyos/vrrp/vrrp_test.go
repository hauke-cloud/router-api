package vrrp

import (
	"slices"
	"testing"
)

func TestParse(t *testing.T) {
	// As printed by a real router, see docs/spike-vyos-container.md.
	out := `Name    Interface      VRID  State      Priority  Last Transition
------  -----------  ------  -------  ----------  -----------------
wan     eth1             10  MASTER          100  5s
lan     eth1.20          20  BACKUP          100  1h2m3s
dmz     eth2             30  FAULT           100  2s
`
	want := []Group{
		{Name: "wan", Interface: "eth1", State: StateMaster},
		{Name: "lan", Interface: "eth1.20", State: StateBackup},
		{Name: "dmz", Interface: "eth2", State: StateFault},
	}
	if got := Parse(out); !slices.Equal(got, want) {
		t.Errorf("Parse = %+v, want %+v", got, want)
	}
}

func TestParseWithoutGroups(t *testing.T) {
	for _, out := range []string{
		"",
		"VRRP data is not available (process not running or no active groups)\n",
		"VRRP data is not available (wait time exceeded)\n",
	} {
		if got := Parse(out); len(got) != 0 {
			t.Errorf("Parse(%q) = %+v", out, got)
		}
	}
}

func TestSummary(t *testing.T) {
	tests := map[string]struct {
		groups     []Group
		wantState  string
		wantMaster bool
		wantFault  bool
	}{
		"none":    {nil, StateNone, false, false},
		"master":  {[]Group{{State: StateMaster}, {State: StateMaster}}, StateMaster, true, false},
		"backup":  {[]Group{{State: StateBackup}}, StateBackup, false, false},
		"mixed":   {[]Group{{State: StateMaster}, {State: StateBackup}}, StateMixed, true, false},
		"faulted": {[]Group{{State: StateMaster}, {State: StateFault}}, StateFault, true, true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			state, master, fault := Summary(tt.groups)
			if state != tt.wantState || master != tt.wantMaster || fault != tt.wantFault {
				t.Errorf("Summary = %s, %v, %v; want %s, %v, %v", state, master, fault, tt.wantState, tt.wantMaster, tt.wantFault)
			}
		})
	}
}
