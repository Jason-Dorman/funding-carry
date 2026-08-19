package db

import (
	"strings"
	"testing"
)

// everyRowType is the full set of tables the system writes to. The integration
// test walks it against information_schema, so a column added to a migration
// without being added here — or the reverse — fails a test rather than a write
// at three in the morning.
func everyRowType() []Row {
	return []Row{
		VenueStateRow{},
		BarRow{},
		BookSnapshotRow{},
		TradesAggRow{},
		FeatureRow{},
		BaseStateRow{},
		AccountStateRow{},
		ProductRow{},
		DecisionRow{},
		PositionRow{},
		FillRow{},
		FundingEventRow{},
		RiskEventRow{},
		FIXSessionRow{},
	}
}

// columns and values are positional. A mismatch would bind the wrong value to
// the wrong column, which Postgres would happily accept whenever the types line
// up — a price landing in a size column is a silent corruption, not an error.
func TestRowColumnsAndValuesLineUp(t *testing.T) {
	t.Parallel()

	for _, r := range everyRowType() {
		d := r.row()
		t.Run(d.table, func(t *testing.T) {
			t.Parallel()

			if d.table == "" {
				t.Fatalf("%T has no table", r)
			}
			if len(d.columns) == 0 {
				t.Fatalf("%s has no columns", d.table)
			}
			if len(d.columns) != len(d.values) {
				t.Fatalf("%s: %d columns but %d values", d.table, len(d.columns), len(d.values))
			}

			seen := make(map[string]bool, len(d.columns))
			for _, c := range d.columns {
				if seen[c] {
					t.Errorf("%s: column %q listed twice", d.table, c)
				}
				seen[c] = true
			}
		})
	}
}

// Every table is written by exactly one row shape, so a producer cannot pick the
// wrong one by accident.
func TestRowTypesCoverDistinctTables(t *testing.T) {
	t.Parallel()

	byTable := make(map[string]int)
	for _, r := range everyRowType() {
		byTable[r.row().table]++
	}
	for table, n := range byTable {
		if n > 1 {
			t.Errorf("%s has %d row types", table, n)
		}
	}
}

// The optional-value helpers are the difference between "the venue did not
// report this" and "the venue reported zero", which the schema records as NULL
// and 0 respectively.
func TestOptionalValuesDistinguishAbsentFromZero(t *testing.T) {
	t.Parallel()

	absent := BarRow{}.row()
	if got := absent.values[len(absent.values)-1]; got != (*int32)(nil) {
		t.Errorf("an unset trade count should be a nil *int32, got %#v", got)
	}

	zero := BarRow{TradeCount: Opt[int32](0)}.row()
	count, ok := zero.values[len(zero.values)-1].(*int32)
	if !ok || count == nil || *count != 0 {
		t.Errorf("a zero trade count should be a pointer to 0, got %#v", zero.values[len(zero.values)-1])
	}

	// An empty optional text column is NULL, not the empty string, which no
	// CHECK constraint in the schema would accept.
	state := VenueStateRow{}.row()
	i := indexOf(state.columns, "funding_source")
	if state.values[i] != nil {
		t.Errorf("an unset funding_source should be NULL, got %#v", state.values[i])
	}
}

func indexOf(columns []string, name string) int {
	for i, c := range columns {
		if c == name {
			return i
		}
	}
	panic("no column named " + name + " in " + strings.Join(columns, ", "))
}

// The writer re-sends a batch whose commit status it could not determine, and
// that is safe only because a repeated insert lands as a no-op. This is the
// assumption behind it, checked mechanically: a table added later without a
// conflict clause would silently turn that retry back into the duplicate-row
// bug, and nothing else in the suite would notice.
func TestEveryRowTypeIsIdempotent(t *testing.T) {
	t.Parallel()

	for _, r := range everyRowType() {
		d := r.row()
		t.Run(d.table, func(t *testing.T) {
			t.Parallel()

			if d.conflict == "" {
				t.Fatalf("%s has no ON CONFLICT clause: the writer's retry would duplicate "+
					"its rows. Give the table the unique key that is its real identity "+
					"(API spec section 5.3), or say here why it has none",
					d.table)
			}
			if !strings.HasPrefix(d.conflict, "ON CONFLICT (") {
				t.Errorf("%s: conflict clause should name its key explicitly, got %q",
					d.table, d.conflict)
			}
			// DO NOTHING or DO UPDATE both make a repeat harmless; a bare
			// ON CONFLICT with neither would not compile as SQL, but an
			// unqualified DO UPDATE that changed a value on every repeat would
			// not be idempotent either.
			if !strings.Contains(d.conflict, "DO NOTHING") && !strings.Contains(d.conflict, "DO UPDATE") {
				t.Errorf("%s: conflict clause resolves to neither DO NOTHING nor DO UPDATE: %q",
					d.table, d.conflict)
			}
		})
	}
}
