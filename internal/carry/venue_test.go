package carry

import (
	"testing"

	"github.com/oklog/ulid/v2"
)

// The state machine, the router and the probe all branch on this: a terminal
// state ends an order, and nothing that follows it may move the order again.
func TestTerminalStates(t *testing.T) {
	t.Parallel()

	cases := []struct {
		state    ExecState
		terminal bool
	}{
		{StateNew, false},
		{StatePartial, false},
		{StateFilled, true},
		{StateCanceled, true},
		{StateRejected, true},
		{ExecState(""), false},
	}
	for _, c := range cases {
		if got := c.state.Terminal(); got != c.terminal {
			t.Errorf("%q.Terminal() = %v, want %v", c.state, got, c.terminal)
		}
	}
}

// An order id is a ULID: parseable, and unique across a burst of mints, which
// is the property a ClOrdID needs and the one a venue enforces by rejecting a
// reuse.
func TestNewOrderIDMintsUniqueULIDs(t *testing.T) {
	t.Parallel()

	seen := make(map[OrderID]bool)
	for range 1000 {
		id := NewOrderID()
		if _, err := ulid.ParseStrict(string(id)); err != nil {
			t.Fatalf("%q is not a ULID: %v", id, err)
		}
		if seen[id] {
			t.Fatalf("%q minted twice", id)
		}
		seen[id] = true
	}
}
