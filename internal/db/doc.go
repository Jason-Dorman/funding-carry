// Package db is the shared persistence layer: the pgx pool, the SQL migrations,
// and the single writer goroutine that owns every insert a binary makes.
//
// The one-writer rule (spec section 11) lives here: producers publish typed rows
// on a channel, the writer batches them, and nothing else touches the database.
//
// Built in Part 2.
package db
