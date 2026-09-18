package carry

import (
	"github.com/oklog/ulid/v2"
)

// NewOrderID mints a ClOrdID.
//
// A ULID rather than a counter or a UUID because it is what the schema already
// assumes (positions.id, ADR-0012) and because it sorts by time: a log of
// ClOrdIDs reads in the order the orders were placed, and a venue that indexes
// on the id sees them arrive in ascending order rather than at random. The
// library's default source is monotonic within a millisecond, so two ids
// minted in the same instant still sort in the order they were minted.
//
// The clock and the entropy are the library's own, not the injected ones the
// rest of carry uses: an id is not a decision, and no test asserts on the value
// of one — tests that need a known id set it directly.
func NewOrderID() OrderID {
	return OrderID(ulid.Make().String())
}
