package credentials

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

type memoryStore struct {
	row        Snapshot
	writes     int
	before     func(*memoryStore)
	failBefore bool
	loseAck    bool
	conflicts  int
}

func (s *memoryStore) Load(context.Context, int) (Snapshot, error) { return s.row, nil }
func (s *memoryStore) CompareAndSwap(_ context.Context, expected Snapshot, data []byte, key *string) (bool, error) {
	s.writes++
	if s.conflicts > 0 {
		s.conflicts--
		s.row.Version++ // Another metadata edit wins this CAS.
		return false, nil
	}
	if s.before != nil {
		f := s.before
		s.before = nil
		f(s)
	}
	if s.failBefore {
		return false, errors.New("database unavailable")
	}
	if s.row.Version != expected.Version || s.row.Deleted {
		return false, nil
	}
	s.row.BizData = data
	s.row.Version++
	if key != nil {
		s.row.Key = *key
	}
	if s.loseAck {
		return false, errors.New("lost response")
	}
	return true, nil
}
func setupRotation() (*memoryStore, Service, Ticket) {
	store := &memoryStore{row: Snapshot{ChannelID: 1, Type: 99, Key: "old", BizData: []byte(`{"vendor":{"future":true},"credentials":{"other":42}}`)}}
	return store, Service{Store: store}, Ticket{ChannelID: 1, Type: 99, AttemptID: "unique-attempt"}
}
func TestRotationResolvesLostDatabaseAcknowledgements(t *testing.T) {
	store, service, ticket := setupRotation()
	store.loseAck = true
	if outcome, err := service.Claim(t.Context(), ticket, time.Now()); err != nil || outcome != ClaimAcquired {
		t.Fatalf("claim=%v err=%v", outcome, err)
	}
	if outcome, err := service.Commit(t.Context(), ticket, "new"); err != nil || outcome != CommitAlreadyApplied {
		t.Fatalf("commit=%v err=%v", outcome, err)
	}
	if store.row.Key != "new" || store.row.Version != 2 || store.writes != 2 {
		t.Fatal("lost response caused extra mutation")
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(store.row.BizData, &data); err != nil {
		t.Fatal(err)
	}
	if string(data["vendor"]) != `{"future":true}` || string(data["credentials"]) != `{"other":42}` {
		t.Fatal("unrelated business data lost")
	}
}

func TestCommitResolvesLostAcknowledgementOnLastAllowedWrite(t *testing.T) {
	store, service, ticket := setupRotation()
	if outcome, err := service.Claim(t.Context(), ticket, time.Now()); err != nil || outcome != ClaimAcquired {
		t.Fatalf("claim=%v err=%v", outcome, err)
	}
	store.conflicts, store.loseAck = 2, true
	if outcome, err := service.Commit(t.Context(), ticket, "new"); err != nil || outcome != CommitAlreadyApplied {
		t.Fatalf("last write outcome=%v err=%v", outcome, err)
	}
	if store.writes != 4 || store.row.Key != "new" || store.row.Version != 4 {
		t.Fatal("final acknowledgement read must not issue another write")
	}
}
func TestCommitRebasesOnlyWhileItOwnsTheOperation(t *testing.T) {
	for _, supersede := range []bool{false, true} {
		t.Run(map[bool]string{false: "metadata", true: "new owner"}[supersede], func(t *testing.T) {
			store, service, ticket := setupRotation()
			if _, err := service.Claim(t.Context(), ticket, time.Now()); err != nil {
				t.Fatal(err)
			}
			store.before = func(s *memoryStore) {
				s.row.Version++
				if supersede {
					s.row.BizData, _ = WriteRefresh(s.row.BizData, &Refresh{AttemptID: "another"})
				} else {
					s.row.BizData = []byte(`{"vendor":{"changed":true},"credentials":{"refresh":{"attempt_id":"unique-attempt","started_at":1}}}`)
				}
			}
			outcome, err := service.Commit(t.Context(), ticket, "new")
			if err != nil {
				t.Fatal(err)
			}
			if supersede {
				if outcome != CommitSuperseded || store.row.Key != "old" {
					t.Fatal("stale owner committed")
				}
			} else {
				if outcome != CommitApplied || store.row.Key != "new" || string(store.row.BizData) != `{"vendor":{"changed":true}}` {
					t.Fatal("metadata change lost")
				}
			}
		})
	}
}
func TestFailedCommitKeepsDurableClaim(t *testing.T) {
	store, service, ticket := setupRotation()
	if _, err := service.Claim(t.Context(), ticket, time.Now()); err != nil {
		t.Fatal(err)
	}
	store.failBefore = true
	if outcome, err := service.Commit(t.Context(), ticket, "new"); err == nil || outcome != CommitStillFenced {
		t.Fatalf("outcome=%v err=%v", outcome, err)
	}
	refresh, err := ReadRefresh(store.row.BizData)
	if err != nil || refresh == nil || refresh.AttemptID != ticket.AttemptID || store.row.Key != "old" {
		t.Fatal("unresolved operation was cleared")
	}
	if store.writes != 4 {
		t.Fatal("database retry must be bounded")
	}
}
func TestCredentialStateRejectsMalformedData(t *testing.T) {
	for _, raw := range []string{`null`, `[]`, `{"credentials":null}`, `{"credentials":{"refresh":{}}}`, `{"credentials":{"refresh":null}}`, `{"credentials":{"refresh":{"attempt_id":3}}}`} {
		t.Run(raw, func(t *testing.T) {
			if _, err := ReadRefresh([]byte(raw)); err == nil {
				t.Fatal("invalid state accepted")
			}
			if err := RequireEditable([]byte(raw)); err == nil {
				t.Fatal("invalid state allowed identity edit")
			}
		})
	}
}
func TestCancelAndRecoveryCannotClearAnotherOwner(t *testing.T) {
	store, service, ticket := setupRotation()
	if _, err := service.Claim(t.Context(), ticket, time.Now()); err != nil {
		t.Fatal(err)
	}
	original := store.row
	ticket.AttemptID = "UNIQUE-ATTEMPT"
	if applied, err := service.Cancel(t.Context(), ticket); err != nil || applied {
		t.Fatal("cancel ignored case-sensitive ownership")
	}
	if err := service.Recover(t.Context(), original, ticket.AttemptID, "new"); !errors.Is(err, ErrConflict) {
		t.Fatal("recovery ignored ownership")
	}
	store.row.Version++
	if err := service.Recover(t.Context(), original, "unique-attempt", "new"); !errors.Is(err, ErrConflict) {
		t.Fatal("recovery ignored snapshot version")
	}
	if store.row.Key != "old" {
		t.Fatal("stale recovery changed key")
	}
}
