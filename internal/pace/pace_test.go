package pace

import (
	"testing"
	"time"
)

func TestEvery(t *testing.T) {
	t.Cleanup(func() { Accelerate(0) })

	if got := Every(15 * time.Second); got != 15*time.Second {
		t.Errorf("unaccelerated: %v", got)
	}
	Accelerate(30)
	if got := Every(15 * time.Second); got != 500*time.Millisecond {
		t.Errorf("accelerated: %v", got)
	}
	// Never so short that a controller spins.
	if got := Every(time.Second); got != 50*time.Millisecond {
		t.Errorf("floor: %v", got)
	}
}
