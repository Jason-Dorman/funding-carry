// Package db is the shared persistence layer: the pgx pool, the SQL migrations,
// the typed rows that enumerate every table the system writes to, and the single
// writer goroutine that owns every insert a binary makes.
//
// The one-writer rule (spec section 11) lives here: producers publish typed rows
// on a channel, the writer batches them, and nothing else touches the database.
//
// Two properties of this package are load-bearing and both are silent when
// broken, so each is a mechanism rather than a convention:
//
//   - Connect registers the shopspring codec on every connection, making
//     decimal.Decimal the native representation of a `numeric` column. Without
//     it a value reaches Go through a float somewhere and loses digits with no
//     error anywhere.
//   - EncodeSnapshot writes decimals into `jsonb` as JSON strings, because
//     Postgres keeps a bare JSON number exact while every consumer outside Go
//     parses it as a double (ADR-0011).
//
// Schema and constraints: docs/api-spec.md section 5.
package db
