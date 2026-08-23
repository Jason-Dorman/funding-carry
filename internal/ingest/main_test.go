package ingest

import (
	"testing"

	"go.uber.org/goleak"
)

// Every test in this package starts goroutines — streams, the sampler, the fake
// WebSocket server — so goroutine leaks are checked for the package as a whole
// rather than test by test (testing strategy: goleak on anything with
// goroutines).
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
