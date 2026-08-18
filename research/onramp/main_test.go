package main

import (
	"testing"

	"go.uber.org/goleak"
)

// The exercise is four goroutines and a channel, so the thing most worth
// asserting is that none of them outlives the test that started it. goleak fails
// the package if one does.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
