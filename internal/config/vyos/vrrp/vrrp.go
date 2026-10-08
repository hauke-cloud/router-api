// Package vrrp reads the VRRP state of a VyOS router.
package vrrp

import (
	"strings"
)

// States a VRRP group can be in, as keepalived names them, and the two
// summaries of several groups.
const (
	StateMaster = "MASTER"
	StateBackup = "BACKUP"
	StateFault  = "FAULT"
	// StateMixed means some groups are master and others backup.
	StateMixed = "MIXED"
	// StateNone means no VRRP group is active.
	StateNone = "NotConfigured"
)

// Group is one VRRP group.
type Group struct {
	Name      string
	Interface string
	State     string
}

// Parse reads the output of "show vrrp". Anything that is not a group row,
// including the message printed when keepalived is not running, is skipped.
func Parse(output string) []Group {
	var groups []Group
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		// Name Interface VRID State Priority Last-Transition
		if len(fields) < 5 {
			continue
		}
		switch fields[3] {
		case StateMaster, StateBackup, StateFault:
			groups = append(groups, Group{Name: fields[0], Interface: fields[1], State: fields[3]})
		}
	}
	return groups
}

// Summary condenses the groups of a router into one state, and says whether
// it is master of anything and whether any group is in fault.
func Summary(groups []Group) (state string, master, fault bool) {
	backup := false
	for _, group := range groups {
		switch group.State {
		case StateMaster:
			master = true
		case StateBackup:
			backup = true
		case StateFault:
			fault = true
		}
	}
	switch {
	case fault:
		state = StateFault
	case master && backup:
		state = StateMixed
	case master:
		state = StateMaster
	case backup:
		state = StateBackup
	default:
		state = StateNone
	}
	return state, master, fault
}
