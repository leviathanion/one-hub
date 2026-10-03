package relay

import (
	"testing"
	"time"
)

type nonDetachableShutdownSession struct {
	relayTestRealtimeSession
	entered, release chan struct{}
}

func (s *nonDetachableShutdownSession) Abort(string)                 { close(s.entered); <-s.release }
func (s *nonDetachableShutdownSession) Detach(string)                {}
func (s *nonDetachableShutdownSession) SupportsGracefulDetach() bool { return false }

func TestRealtimeCoordinatorJoinsNonDetachableSessionForEveryCloseSource(t *testing.T) {
	for _, source := range []string{"supplier", "ctx", "external", "user"} {
		t.Run(source, func(t *testing.T) {
			session := &nonDetachableShutdownSession{entered: make(chan struct{}), release: make(chan struct{})}
			actor := newRealtimeRelayActor(nil, session, time.Second)
			defer actor.cancel()
			defer close(session.release)
			actor.exitCh <- realtimeRelayExit{source: source, graceful: true}
			go actor.coordinate()
			select {
			case <-session.entered:
			case <-actor.done:
				t.Fatal("coordinator did not join provider finalization")
			case <-time.After(time.Second):
				t.Fatal("coordinator stuck")
			}
			select {
			case <-actor.done:
				t.Fatal("reported done before provider finalization")
			default:
			}
		})
	}
}
