package db

import (
	"testing"

	"go.uber.org/goleak"
)

// The writer is a goroutine that outlives every producer in the binary, so a
// leak here is a leak in every service. beforeTests is a no-op unless the
// integration build tag is set, in which case it prepares a throwaway database.
func TestMain(m *testing.M) {
	beforeTests()
	goleak.VerifyTestMain(m)
}
