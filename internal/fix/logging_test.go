package fix

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/quickfixgo/quickfix"
)

// recordingLog is the file log the tee wraps.
type recordingLog struct{ events, msgs []string }

func (r *recordingLog) OnIncoming(b []byte)         { r.msgs = append(r.msgs, string(b)) }
func (r *recordingLog) OnOutgoing(b []byte)         { r.msgs = append(r.msgs, string(b)) }
func (r *recordingLog) OnEvent(s string)            { r.events = append(r.events, s) }
func (r *recordingLog) OnEventf(f string, a ...any) { r.events = append(r.events, f) }

type recordingLogFactory struct{ log *recordingLog }

func (f recordingLogFactory) Create() (quickfix.Log, error) { return f.log, nil }
func (f recordingLogFactory) CreateSessionLog(quickfix.SessionID) (quickfix.Log, error) {
	return f.log, nil
}

// Events reach both the file and the structured log; messages reach the file
// only. The event that motivated this — a Logout the initiator never sees as
// an application — is the one asserted on.
func TestQuickfixEventsAreTeedIntoTheStructuredLog(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	inner := &recordingLog{}
	factory := newTeeLogFactory(recordingLogFactory{inner}, slog.New(slog.NewTextHandler(&out, nil)))
	l, err := factory.CreateSessionLog(testSessionID())
	if err != nil {
		t.Fatal(err)
	}

	l.OnIncoming([]byte("8=FIX.4.4|35=0|"))
	l.OnEventf("Invalid Session State: Received Msg %s while waiting for Logon", "58=MsgSeqNum too low")
	l.OnEvent("Disconnected")

	if len(inner.msgs) != 1 || len(inner.events) != 2 {
		t.Fatalf("file log got %d messages and %d events, want 1 and 2", len(inner.msgs), len(inner.events))
	}
	got := out.String()
	if !strings.Contains(got, "MsgSeqNum too low") || !strings.Contains(got, "Disconnected") {
		t.Errorf("structured log is missing the events:\n%s", got)
	}
	if strings.Contains(got, "35=0") {
		t.Errorf("a message reached the structured log; messages belong in the file only:\n%s", got)
	}
	if !strings.Contains(got, "session=FIX.4.4:SIMV->CARRY") {
		t.Errorf("events are not labelled with the session:\n%s", got)
	}
}
