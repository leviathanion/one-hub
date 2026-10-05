package credentials

import (
	"context"
	"errors"
	"strings"
	"time"
)

var ErrConflict = errors.New("渠道凭证已被并发更新，请重新授权")

type Snapshot struct {
	ChannelID     int
	Type          int
	Key           string
	Version       uint64
	InternalState []byte
	Deleted       bool
}

// Store performs only authoritative reads and single-row atomic CAS. Secret
// values must never be logged. A CAS miss is distinct from a database error.
type Store interface {
	Load(context.Context, int) (Snapshot, error)
	CompareAndSwap(context.Context, Snapshot, []byte, *string) (bool, error)
}

type Service struct{ Store Store }

type Ticket struct {
	ChannelID       int
	Type            int
	AttemptID       string
	ExpectedVersion uint64
}

type ClaimOutcome int

const (
	ClaimAcquired ClaimOutcome = iota
	ClaimBusy
	ClaimSuperseded
)

type CommitOutcome int

const (
	CommitApplied CommitOutcome = iota
	CommitAlreadyApplied
	CommitSuperseded
	CommitStillFenced
)

func (s Service) Claim(ctx context.Context, ticket Ticket, startedAt time.Time) (ClaimOutcome, error) {
	if ticket.ChannelID <= 0 || strings.TrimSpace(ticket.AttemptID) == "" {
		return ClaimSuperseded, ErrConflict
	}
	row, err := s.Store.Load(ctx, ticket.ChannelID)
	if err != nil {
		return ClaimSuperseded, err
	}
	if row.Deleted || row.Type != ticket.Type {
		return ClaimSuperseded, nil
	}
	refresh, err := ReadRefresh(row.InternalState)
	if err != nil {
		return ClaimSuperseded, err
	}
	if refresh != nil {
		if refresh.AttemptID == ticket.AttemptID {
			return ClaimAcquired, nil
		}
		return ClaimBusy, nil
	}
	if row.Version != ticket.ExpectedVersion {
		return ClaimSuperseded, nil
	}
	data, err := WriteRefresh(row.InternalState, &Refresh{AttemptID: ticket.AttemptID, StartedAt: startedAt.Unix()})
	if err != nil {
		return ClaimSuperseded, err
	}
	applied, writeErr := s.Store.CompareAndSwap(ctx, row, data, nil)
	if writeErr == nil && applied {
		return ClaimAcquired, nil
	}
	latest, readErr := s.Store.Load(ctx, ticket.ChannelID)
	if readErr != nil {
		return ClaimSuperseded, errors.Join(writeErr, readErr)
	}
	refresh, err = ReadRefresh(latest.InternalState)
	if err != nil {
		return ClaimSuperseded, errors.Join(writeErr, err)
	}
	if !latest.Deleted && latest.Type == ticket.Type && refresh != nil {
		if refresh.AttemptID == ticket.AttemptID {
			return ClaimAcquired, nil
		}
		return ClaimBusy, writeErr
	}
	return ClaimSuperseded, writeErr
}

// Commit retries only the DB CAS, never the external credential exchange.
// Metadata edits may advance Version while the attempt remains the owner.
func (s Service) Commit(ctx context.Context, ticket Ticket, key string) (CommitOutcome, error) {
	if strings.TrimSpace(key) == "" || ticket.AttemptID == "" {
		return CommitSuperseded, ErrConflict
	}
	var lastErr error
	for attempt := 0; ; attempt++ {
		row, err := s.Store.Load(ctx, ticket.ChannelID)
		if err != nil {
			return CommitStillFenced, errors.Join(lastErr, err)
		}
		if row.Deleted || row.Type != ticket.Type {
			return CommitSuperseded, nil
		}
		refresh, err := ReadRefresh(row.InternalState)
		if err != nil {
			return CommitStillFenced, err
		}
		if refresh == nil {
			if row.Key == key && row.Version > ticket.ExpectedVersion {
				return CommitAlreadyApplied, nil
			}
			return CommitSuperseded, nil
		}
		if refresh.AttemptID != ticket.AttemptID {
			return CommitSuperseded, nil
		}
		// The last iteration resolves a lost write acknowledgement without
		// issuing another CAS. External credential exchange is never retried.
		if attempt == 3 {
			return CommitStillFenced, lastErr
		}
		data, err := WriteRefresh(row.InternalState, nil)
		if err != nil {
			return CommitStillFenced, err
		}
		applied, err := s.Store.CompareAndSwap(ctx, row, data, &key)
		if err == nil && applied {
			return CommitApplied, nil
		}
		lastErr = err
	}
}

// Cancel is only legal when the caller proved that the old refresh token was
// not dispatched. Elapsed time is never evidence that cancellation is safe.
func (s Service) Cancel(ctx context.Context, ticket Ticket) (bool, error) {
	if ticket.AttemptID == "" {
		return false, ErrConflict
	}
	for attempt := 0; attempt < 3; attempt++ {
		row, err := s.Store.Load(ctx, ticket.ChannelID)
		if err != nil {
			return false, err
		}
		if row.Deleted || row.Type != ticket.Type {
			return false, nil
		}
		refresh, err := ReadRefresh(row.InternalState)
		if err != nil {
			return false, err
		}
		if refresh == nil {
			return false, nil
		}
		if refresh.AttemptID != ticket.AttemptID {
			return false, nil
		}
		data, err := WriteRefresh(row.InternalState, nil)
		if err != nil {
			return false, err
		}
		applied, err := s.Store.CompareAndSwap(ctx, row, data, nil)
		if err != nil {
			return false, err
		}
		if applied {
			return true, nil
		}
	}
	return false, ErrConflict
}

// Recover accepts an independently authorized credential. The provider validates
// both account identities against this exact snapshot before calling it.
func (s Service) Recover(ctx context.Context, expected Snapshot, attemptID, key string) error {
	if expected.Deleted || attemptID == "" || strings.TrimSpace(key) == "" {
		return ErrConflict
	}
	refresh, err := ReadRefresh(expected.InternalState)
	if err != nil {
		return err
	}
	if refresh == nil || refresh.AttemptID != attemptID {
		return ErrConflict
	}
	data, err := WriteRefresh(expected.InternalState, nil)
	if err != nil {
		return err
	}
	applied, err := s.Store.CompareAndSwap(ctx, expected, data, &key)
	if err != nil {
		return err
	}
	if !applied {
		return ErrConflict
	}
	return nil
}
