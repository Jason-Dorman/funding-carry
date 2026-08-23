package ingest

import (
	"context"
	"errors"
	"testing"

	"github.com/Jason-Dorman/funding-carry/internal/db"
)

func TestSubmitTranslatesAStoppedWriter(t *testing.T) {
	sink := &fakeSink{}
	sink.stop()

	err := submit(context.Background(), sink, db.BarRow{})

	// This is the one failure a stream does not retry. Left as an ordinary feed
	// error it would be counted as a gap and the stream would reconnect forever
	// against a writer that has stopped accepting rows, so shutdown would never
	// finish (TestStreamStopsWhenTheWriterIsGone is the other half of this).
	if !errors.Is(err, errWriterGone) {
		t.Fatalf("submit to a stopped writer returned %v, want errWriterGone", err)
	}
}

func TestSubmitPassesOtherFailuresThrough(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := submit(ctx, &fakeSink{}, db.BarRow{})
	if err == nil || errors.Is(err, errWriterGone) {
		t.Fatalf("submit on a canceled context returned %v, want the context error", err)
	}
}
