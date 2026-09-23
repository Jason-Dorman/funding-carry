package venue

import (
	"testing"

	"go.uber.org/goleak"
)

// Run is a goroutine that lives as long as the binary; a leak here is a leak
// in carry.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
