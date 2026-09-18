package fix

import (
	"fmt"
	"log/slog"

	"github.com/quickfixgo/quickfix"
)

// quickfix keeps two logs per session: every message verbatim, and a short
// event log — connects, logons, disconnects, and the reasons for them. Both
// go to files under the store path. The messages belong there; the events do
// not belong *only* there, and the first live contact between carry and
// sim-venue showed why. The venue refused every logon with "MsgSeqNum too
// low, expecting 31 but received 6", a Logout it sends while the initiator is
// still waiting for its own Logon to be answered — a state in which quickfix
// handles the message itself and never hands it to the application. carry's
// own log showed a logout every five seconds and nothing else; the one line
// that explained it was in a file inside the container.
//
// teeLogFactory keeps quickfix's file log and copies its events into the
// binary's structured log, so the reason a session is not up is where an
// operator looks. Events are rare on a healthy session — none at all once it
// is logged on — and loud on a flapping one, which is the point.

type teeLogFactory struct {
	inner quickfix.LogFactory
	log   *slog.Logger
}

func newTeeLogFactory(inner quickfix.LogFactory, log *slog.Logger) quickfix.LogFactory {
	return teeLogFactory{inner: inner, log: log.With("engine", "quickfix")}
}

func (f teeLogFactory) Create() (quickfix.Log, error) {
	inner, err := f.inner.Create()
	if err != nil {
		return nil, err
	}
	return teeLog{inner: inner, log: f.log}, nil
}

func (f teeLogFactory) CreateSessionLog(sessionID quickfix.SessionID) (quickfix.Log, error) {
	inner, err := f.inner.CreateSessionLog(sessionID)
	if err != nil {
		return nil, err
	}
	return teeLog{inner: inner, log: f.log.With("session", sessionID.String())}, nil
}

type teeLog struct {
	inner quickfix.Log
	log   *slog.Logger
}

// OnIncoming and OnOutgoing go to the file only: every heartbeat is a
// message, and the message log is what the resend demo and the replay read.
func (l teeLog) OnIncoming(b []byte) { l.inner.OnIncoming(b) }
func (l teeLog) OnOutgoing(b []byte) { l.inner.OnOutgoing(b) }

func (l teeLog) OnEvent(s string) {
	l.inner.OnEvent(s)
	l.log.Info("fix engine event", "event", s)
}

func (l teeLog) OnEventf(format string, a ...any) {
	l.inner.OnEventf(format, a...)
	l.log.Info("fix engine event", "event", fmt.Sprintf(format, a...))
}
