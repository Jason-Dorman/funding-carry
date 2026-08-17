//go:build !integration

package db

// beforeTests does nothing for the unit suite: `make test` must run with no
// database, no network and no Compose stack.
func beforeTests() {}
