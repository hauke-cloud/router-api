// Package pace is the one knob on how often the controllers look again at
// things nobody notifies them about.
//
// Core cannot watch provider objects (it does not know their kinds), and
// nothing announces that a server has finished booting, so a good part of the
// system advances on timers. In production those are tens of seconds. A test
// that runs every manager together would take most of an hour that way, so it
// divides them.
package pace

import (
	"sync/atomic"
	"time"
)

var divisor atomic.Int64

// Every returns how long to wait before looking again, given the interval
// meant for production.
func Every(interval time.Duration) time.Duration {
	if d := divisor.Load(); d > 1 {
		return max(interval/time.Duration(d), 50*time.Millisecond)
	}
	return interval
}

// Accelerate divides every interval by factor. It is for tests; nothing in
// the managers calls it.
func Accelerate(factor int64) {
	divisor.Store(factor)
}
