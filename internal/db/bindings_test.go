package db

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

var updateGolden = flag.Bool("update", false, "rewrite testdata/row_bindings.json")

// goldenBindings is testdata/row_bindings.json: for every table, which struct
// field supplies which column.
//
// This is the only thing standing between the schema and a transposition. Two
// same-typed columns swapped in a row() method — the futures and spot cash
// balances, buy and sell volume, the two impact prices — bind cleanly, insert
// cleanly, and are accepted by Postgres, because nothing about a numeric column
// says which numeric belongs in it. TestRowColumnsAndValuesLineUp compares slice
// lengths and TestRowTypesMatchTheSchema compares name sets; both are blind to
// order, and every value-level test in the package covers only two of the
// fourteen row types. Regenerate with `go test ./internal/db -update` and read
// the diff: a changed line here is a changed meaning of a column.
const goldenPath = "testdata/row_bindings.json"

func TestRowValuesBindToTheRightColumns(t *testing.T) {
	t.Parallel()

	got := map[string]map[string]string{}
	for _, r := range everyRowType() {
		table, binding := bindingOf(t, r)
		got[table] = binding
	}

	if *updateGolden {
		writeGolden(t, got)
		t.Log("wrote " + goldenPath)
		return
	}

	raw, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden (regenerate with -update): %v", err)
	}
	var want map[string]map[string]string
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatalf("parse golden: %v", err)
	}

	for table, wantCols := range want {
		gotCols, ok := got[table]
		if !ok {
			t.Errorf("%s: in the golden file but no row type produces it", table)
			continue
		}
		for column, wantField := range wantCols {
			gotField, ok := gotCols[column]
			if !ok {
				t.Errorf("%s.%s: golden expects it to carry %s, but the row type no longer writes that column",
					table, column, wantField)
				continue
			}
			if gotField != wantField {
				t.Errorf("%s.%s is now bound to %s, was %s — a column changed meaning",
					table, column, gotField, wantField)
			}
		}
		for column, gotField := range gotCols {
			if _, ok := wantCols[column]; !ok {
				t.Errorf("%s.%s (from %s) is new; regenerate the golden and review the diff",
					table, column, gotField)
			}
		}
	}
	for table := range got {
		if _, ok := want[table]; !ok {
			t.Errorf("%s: new table; regenerate the golden and review the diff", table)
		}
	}
}

// bindingOf fills every field of a row struct with a value unique to that field,
// calls row(), and reads back which field's value landed in each column
// position. That is what makes a transposition visible: swapping two values
// swaps the two field names in the result.
func bindingOf(t *testing.T, r Row) (string, map[string]string) {
	t.Helper()

	typ := reflect.TypeOf(r)
	filled := reflect.New(typ).Elem()
	bySentinel := map[string]string{}
	for i := range typ.NumField() {
		field := typ.Field(i)
		value, sentinel := sentinelFor(t, field.Type, i+1)
		filled.Field(i).Set(value)
		if previous, clash := bySentinel[sentinel]; clash {
			t.Fatalf("%s: %s and %s produce the same sentinel %q, so a swap between them "+
				"would be invisible here", typ.Name(), previous, field.Name, sentinel)
		}
		bySentinel[sentinel] = field.Name
	}

	d := filled.Interface().(Row).row()
	binding := map[string]string{}
	for i, column := range d.columns {
		field, ok := bySentinel[render(d.values[i])]
		if !ok {
			t.Fatalf("%s.%s: value %#v matches no field's sentinel — the row method is "+
				"transforming it in a way this test cannot follow", d.table, column, d.values[i])
		}
		binding[column] = field
	}
	return d.table, binding
}

// sentinelFor builds a value unique to a field's position, and the string it
// renders as once row() has passed it through the optional-value helpers.
func sentinelFor(t *testing.T, typ reflect.Type, n int) (reflect.Value, string) {
	t.Helper()

	// A distinct decimal per field: n thousandths is unique, non-zero, and
	// survives the Num/NullDecimal wrappers unchanged.
	dec := decimal.New(int64(n), -3)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	switch typ {
	case reflect.TypeOf(decimal.Decimal{}):
		return reflect.ValueOf(dec), render(dec)
	case reflect.TypeOf(decimal.NullDecimal{}):
		v := Num(dec)
		return reflect.ValueOf(v), render(v)
	case reflect.TypeOf(time.Time{}):
		v := base.Add(time.Duration(n) * time.Hour)
		return reflect.ValueOf(v), render(v)
	case reflect.TypeOf(Snapshot(nil)):
		v := Snapshot(fmt.Sprintf(`{"field":%d}`, n))
		return reflect.ValueOf(v), render(v)
	case reflect.TypeOf([]string(nil)):
		v := []string{fmt.Sprintf("REASON_%d", n)}
		return reflect.ValueOf(v), render(v)
	case reflect.TypeOf([]decimal.Decimal(nil)):
		v := []decimal.Decimal{dec}
		return reflect.ValueOf(v), render(v)
	}

	switch typ.Kind() {
	case reflect.String:
		// Closed-vocabulary columns never reach the database here, so an
		// arbitrary unique string is safe and keeps every field distinguishable.
		v := reflect.ValueOf(fmt.Sprintf("field-%d", n)).Convert(typ)
		return v, render(v.Interface())
	case reflect.Bool:
		// Only ever one bool per row type, so a shared value cannot hide a swap.
		// The clash check in bindingOf enforces that assumption.
		return reflect.ValueOf(true), render(true)
	case reflect.Int32, reflect.Int64:
		v := reflect.ValueOf(int64(n)).Convert(typ)
		return v, render(v.Interface())
	case reflect.Pointer:
		inner, _ := sentinelFor(t, typ.Elem(), n)
		p := reflect.New(typ.Elem())
		p.Elem().Set(inner)
		return p, render(p.Interface())
	}

	t.Fatalf("no sentinel for %s; add one so this type cannot be transposed unnoticed", typ)
	return reflect.Value{}, ""
}

// render collapses a bound value to a comparable string, following pointers so a
// *int32 and the int32 it points at compare equal.
func render(v any) string {
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return "<nil>"
		}
		return fmt.Sprint(rv.Elem().Interface())
	}
	return fmt.Sprint(v)
}

func writeGolden(t *testing.T, bindings map[string]map[string]string) {
	t.Helper()

	// Sorted keys so the committed file has a stable, reviewable diff.
	sorted := map[string]map[string]string{}
	tables := make([]string, 0, len(bindings))
	for table := range bindings {
		tables = append(tables, table)
	}
	sort.Strings(tables)
	for _, table := range tables {
		sorted[table] = bindings[table]
	}

	out, err := json.MarshalIndent(sorted, "", "  ")
	if err != nil {
		t.Fatalf("marshal golden: %v", err)
	}
	if err := os.WriteFile(goldenPath, append(out, '\n'), 0o644); err != nil {
		t.Fatalf("write golden: %v", err)
	}
}
