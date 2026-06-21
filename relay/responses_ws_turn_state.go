package relay

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"one-api/common/responsesws"
	"one-api/common/wsconn"
	"one-api/middleware"
	"one-api/types"
)

type responsesWSIOState struct {
	bridge *ResponsesWSIOBridge
	client *wsconn.ManagedConn
}

type responsesWSSnapshotState struct {
	snapshot *ResponsesWSRequestSnapshot
	mu       sync.RWMutex
}

type responsesWSLeaseState struct {
	mu           sync.Mutex
	pendingLease middleware.ResponsesWSLease
	activeLease  middleware.ResponsesWSLease
}

type responsesWSUpstreamState struct {
	sessionGeneration string
	channelID         int
	session           responsesws.Upstream
	recvArmed         bool
}

// Slots are value fields instead of pointer fields so zero-value actor fixtures
// stay safe during lifecycle tests. The lifecycle invariant is still explicit:
// a slot is live only when its identity field is populated (openingID, attempt,
// or active attempt), and transitions go through the helpers below.
type responsesWSTurnSlots struct {
	opening responsesWSOpeningTurn
	pending responsesWSPendingTurn
	active  responsesWSActiveTurn

	history responsesWSTurnHistory
}

type responsesWSTurnHistory struct {
	lastFinal                  *types.OpenAIResponsesResponses
	recentFinalizedResponseIDs []string
}

type responsesWSOpeningTurn struct {
	openingID  string
	firstFrame *responsesws.RawResponsesCreateFrame
	startedAt  time.Time
	admission  *ResponsesWSTurnAdmission
}

type responsesWSPendingTurn struct {
	phase     responsesWSPendingTurnPhase
	openingID string

	attempt *ResponsesWSTurnAttempt

	provider responsesWSPendingProviderState
	cancel   responsesWSPendingCancelState
}

type responsesWSPendingProviderState struct {
	journal responsesWSProviderJournal

	bridgeOpenLocalErrorAttemptID string
	bridgeOpenProviderErr         *ResponsesWSEventBridgeOpenProviderError
}

type responsesWSPendingCancelState struct {
	sendAttemptID string
	sendCancel    context.CancelFunc

	createAttemptID string
	createFrame     responsesws.Frame
}

type responsesWSActiveTurn struct {
	attempt   *ResponsesWSTurnAttempt
	evidence  responsesws.ProviderSettlementLogProjection
	affinity  *ResponsesTurnAffinity
	channelID int

	bridgeCancelPendingAttemptID string
}

type responsesWSTurnFinalization struct {
	attemptID string
}

type responsesWSPendingCleanup struct {
	attempt   *ResponsesWSTurnAttempt
	openingID string
	phase     responsesWSPendingTurnPhase
	provider  responsesWSPendingProviderState
	cancel    responsesWSPendingCancelState
}

type responsesWSProviderJournal struct {
	entries []responsesWSProviderJournalEntry
	bytes   int
}

type responsesWSProviderJournalEntry struct {
	Observation responsesws.ProviderObservation

	Downstream *ResponsesWSEventProviderDownstream
	Failure    *ResponsesWSEventProviderRecvFailed
}

type responsesWSProviderJournalAppendResult struct {
	Buffered  bool
	OverLimit bool
}

func (j *responsesWSProviderJournal) appendEntry(entry responsesWSProviderJournalEntry, replayBytes int, maxBytes int, enforceBytes bool) responsesWSProviderJournalAppendResult {
	if j == nil || entry.Observation.IsZero() {
		return responsesWSProviderJournalAppendResult{}
	}
	overLimit := len(j.entries) >= responsesWSPendingProviderEventsMax
	if enforceBytes && j.bytes+replayBytes > maxBytes {
		overLimit = true
	}
	if overLimit {
		// Keep the triggering observation in the journal before failing closed so
		// settlement projection cannot accidentally turn a provider activity into
		// a rollback. The replay payload is intentionally dropped on overflow.
		entry.Downstream = nil
		entry.Failure = nil
	} else if entry.Downstream != nil {
		j.bytes += replayBytes
	}
	j.entries = append(j.entries, entry)
	return responsesWSProviderJournalAppendResult{Buffered: !overLimit, OverLimit: overLimit}
}

func (j *responsesWSProviderJournal) AppendDownstream(event ResponsesWSEventProviderDownstream, upstream responsesws.UpstreamEvent, maxBytes int) (bool, bool) {
	if j == nil {
		return false, false
	}
	entry := responsesWSProviderJournalEntry{Observation: responsesws.NewProviderObservation(upstream)}
	eventBytes := len(responsesWSProviderDownstreamPayload(event))
	copied := event
	entry.Downstream = &copied
	result := j.appendEntry(entry, eventBytes, maxBytes, true)
	return result.Buffered, result.OverLimit
}

func (j *responsesWSProviderJournal) AppendFailure(event ResponsesWSEventProviderRecvFailed, upstream responsesws.UpstreamEvent) bool {
	if j == nil {
		return false
	}
	copied := event
	result := j.appendEntry(responsesWSProviderJournalEntry{
		Observation: responsesws.NewProviderObservation(upstream),
		Failure:     &copied,
	}, 0, 0, false)
	return result.OverLimit
}

func (j *responsesWSProviderJournal) AppendDownstreamReplay(event ResponsesWSEventProviderDownstream) {
	if j == nil {
		return
	}
	copied := event
	j.entries = append(j.entries, responsesWSProviderJournalEntry{
		Observation: responsesws.NewProviderObservation(upstreamEventFromProviderDownstream(event)),
		Downstream:  &copied,
	})
	j.bytes += len(responsesWSProviderDownstreamPayload(event))
}

func (j *responsesWSProviderJournal) AppendFailureReplay(event ResponsesWSEventProviderRecvFailed) {
	if j == nil {
		return
	}
	copied := event
	j.entries = append(j.entries, responsesWSProviderJournalEntry{
		Observation: responsesws.NewProviderObservation(upstreamEventFromProviderRecvFailed(event)),
		Failure:     &copied,
	})
}

func (j *responsesWSProviderJournal) AppendLifecycle(upstream responsesws.UpstreamEvent) bool {
	if j == nil {
		return false
	}
	obs := responsesws.NewProviderObservation(upstream)
	if obs.IsZero() {
		return false
	}
	return j.AppendObservation(obs)
}

func (j *responsesWSProviderJournal) AppendObservation(obs responsesws.ProviderObservation) bool {
	if j == nil || obs.IsZero() {
		return false
	}
	result := j.appendEntry(responsesWSProviderJournalEntry{Observation: obs}, 0, 0, false)
	return result.OverLimit
}

func (j *responsesWSProviderJournal) Project() responsesws.ProviderSettlementLogProjection {
	if j == nil || len(j.entries) == 0 {
		return responsesws.ProviderSettlementLogProjection{}
	}
	var out responsesws.ProviderSettlementLogProjection
	for _, entry := range j.entries {
		out.Observe(entry.Observation)
	}
	return out
}

func (j *responsesWSProviderJournal) DownstreamEvents() []ResponsesWSEventProviderDownstream {
	if j == nil || len(j.entries) == 0 {
		return nil
	}
	out := make([]ResponsesWSEventProviderDownstream, 0, len(j.entries))
	for _, entry := range j.entries {
		if entry.Downstream != nil {
			out = append(out, *entry.Downstream)
		}
	}
	return out
}

func (j *responsesWSProviderJournal) Failures() []ResponsesWSEventProviderRecvFailed {
	if j == nil || len(j.entries) == 0 {
		return nil
	}
	out := make([]ResponsesWSEventProviderRecvFailed, 0, len(j.entries))
	for _, entry := range j.entries {
		if entry.Failure != nil {
			out = append(out, *entry.Failure)
		}
	}
	return out
}

func (j *responsesWSProviderJournal) Replay() []responsesWSProviderJournalEntry {
	if j == nil || len(j.entries) == 0 {
		return nil
	}
	out := make([]responsesWSProviderJournalEntry, 0, len(j.entries))
	for _, entry := range j.entries {
		if entry.Downstream == nil && entry.Failure == nil {
			continue
		}
		out = append(out, entry)
	}
	return out
}

func (s *responsesWSTurnSlots) BeginOpening(opening responsesWSOpeningTurn) error {
	if s == nil {
		return errors.New("turn slots are required")
	}
	if s.pending.attempt != nil || s.active.attempt != nil {
		return errors.New("cannot begin opening while a turn is pending or active")
	}
	opening.openingID = strings.TrimSpace(opening.openingID)
	if opening.openingID == "" {
		return errors.New("opening id is required")
	}
	s.opening = opening
	s.pending = responsesWSPendingTurn{
		phase:     responsesWSPendingTurnOpening,
		openingID: opening.openingID,
	}
	return nil
}

func (s *responsesWSTurnSlots) AttachPending(pending responsesWSPendingTurn) error {
	if s == nil {
		return errors.New("turn slots are required")
	}
	if s.active.attempt != nil {
		return errors.New("cannot attach pending while active turn exists")
	}
	if pending.attempt != nil && strings.TrimSpace(pending.openingID) == "" {
		pending.openingID = pending.attempt.OpeningID
	}
	s.pending = pending
	return nil
}

func (s *responsesWSTurnSlots) CommitPendingToActive(channelID int) (responsesWSActiveTurn, []responsesWSProviderJournalEntry, error) {
	if s == nil || s.pending.attempt == nil {
		return responsesWSActiveTurn{}, nil, errors.New("pending attempt is required")
	}
	if s.active.attempt != nil {
		return responsesWSActiveTurn{}, nil, errors.New("cannot commit pending while active turn exists")
	}
	pending := s.pending
	active := responsesWSActiveTurn{
		attempt:   pending.attempt,
		evidence:  pending.provider.journal.Project(),
		affinity:  CommitResponsesTurnAffinity(pending.attempt.Candidate, channelID),
		channelID: channelID,
	}
	replay := pending.provider.journal.Replay()
	s.active = active
	s.pending = responsesWSPendingTurn{cancel: pending.cancel}
	return active, replay, nil
}

func (s *responsesWSTurnSlots) FinishActive(result responsesWSTurnFinalization) error {
	if s == nil {
		return errors.New("turn slots are required")
	}
	if s.active.attempt == nil {
		return nil
	}
	if result.attemptID != "" && s.active.attempt != nil && s.active.attempt.AttemptID != result.attemptID {
		return errors.New("active attempt mismatch")
	}
	s.active = responsesWSActiveTurn{}
	return nil
}

func (s *responsesWSTurnSlots) ClearPending() responsesWSPendingCleanup {
	if s == nil {
		return responsesWSPendingCleanup{}
	}
	cleanup := responsesWSPendingCleanup{
		attempt:   s.pending.attempt,
		openingID: s.pending.openingID,
		phase:     s.pending.phase,
		provider:  s.pending.provider,
		cancel:    s.pending.cancel,
	}
	s.pending = responsesWSPendingTurn{}
	return cleanup
}

func (s *responsesWSTurnSlots) ResetPendingProvider() {
	if s == nil {
		return
	}
	s.pending.provider = responsesWSPendingProviderState{}
}

type responsesWSWorkerState struct {
	runWG           sync.WaitGroup
	sendCommands    chan responsesWSSendCommand
	controlCommands chan responsesWSSendCommand
	sendOnce        sync.Once
	controlOnce     sync.Once

	setupCancelMu sync.Mutex
	setupCancel   context.CancelFunc
}

type responsesWSCloseState struct {
	closed              atomic.Bool
	clientClosed        atomic.Bool
	closeIntentPosted   atomic.Bool
	downstreamCloseSent atomic.Bool
	backpressurePosted  atomic.Bool
}

type responsesWSWatchdogState struct {
	lastActivityMu sync.Mutex
	lastActivity   time.Time

	activeTurnMu       sync.Mutex
	activeTurnTimer    *time.Timer
	activeTurnTimerGen int64

	busyRejectWindowStart time.Time
	busyRejects           int
}
