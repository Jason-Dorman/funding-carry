package ingest

import (
	"context"
	"errors"

	"github.com/Jason-Dorman/funding-carry/internal/db"
)

// Sink is where decoded rows go. It is the writer, named here as a one-method
// interface because internal/ingest is its consumer (architecture section 12):
// every handler in this package can then be tested against a slice of rows
// instead of a database.
//
// Submit blocks when the writer's queue is full. That is the backpressure the
// one-writer design depends on, and it is why the stream goroutines must never
// hold anything that a slow database could turn into a leak.
type Sink interface {
	Submit(ctx context.Context, r db.Row) error
}

// submit forwards a row and translates the one failure that is not a feed
// problem. db.ErrWriterStopped means the writer has gone, so there is nothing
// left for this stream to record into and reconnecting would not help; every
// other error is the caller's to log and carry on from.
func submit(ctx context.Context, s Sink, r db.Row) error {
	if err := s.Submit(ctx, r); err != nil {
		if errors.Is(err, db.ErrWriterStopped) {
			return errWriterGone
		}
		return err
	}
	return nil
}
