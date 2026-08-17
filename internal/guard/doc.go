// Package guard holds module-wide invariant checks that run as ordinary tests.
//
// It contains no production code. Its tests type-check every package in the
// module and fail the build when code violates a rule that the compiler and the
// available linters cannot express — currently the two ways decimal arithmetic
// goes silently wrong (see decimal_test.go).
//
// A rule belongs here only when breaking it is silent. Anything a linter already
// catches belongs in .golangci.yml instead.
package guard
