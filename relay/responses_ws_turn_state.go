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
	providersBase "one-api/providers/base"
	"one-api/types"
)

type responsesWSIOState struct {
	pump   *ResponsesWSIOPump
	client *wsconn.ManagedConn
}

type responsesWSSnapshotState struct {
	snapshot *ResponsesWSRequestSnapshot
	mu       sync.RWMutex
}

type responsesWSLeaseState struct {
	mu           sync.Mutex
	pendingLease middleware.ResponsesWSLease
	pendingBytes middleware.ResponsesWSByteLease
	activeLease  middleware.ResponsesWSLease
}

type responsesWSUpstreamState struct {
	sessionGeneration string
	channelID         int
	session           responsesws.Upstream
	provider          providersBase.ProviderInterface
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
	queue   responsesWSCreateQueue
	inject  responsesWSInjectState

	history responsesWSTurnHistory
}

type responsesWSTurnHistory struct {
	lastFinal                  *types.OpenAIResponsesResponses
	recentFinalizedResponseIDs []string
	localEphemeralResponseIDs  []string
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

	attempt        *ResponsesWSTurnAttempt
	sendCompletion <-chan ResponsesWSEventSendResult

	provider responsesWSPendingProviderState
}

type responsesWSPendingProviderState struct {
	journal responsesWSProviderJournal
}

type responsesWSActiveTurn struct {
	attempt                 *ResponsesWSTurnAttempt
	evidence                responsesws.ProviderActivityProjection
	affinity                *ResponsesTurnAffinity
	channelID               int
	lastProviderSequence    int64
	hasLastProviderSequence bool
}

type responsesWSQueuedCreate struct {
	payload    []byte
	receivedAt time.Time
}

type responsesWSCreateQueue struct {
	items []responsesWSQueuedCreate
	bytes int
}

func (q *responsesWSCreateQueue) Push(event ResponsesWSEventClientFrame, maxFrames, maxBytes int) bool {
	if q == nil || maxFrames <= 0 || maxBytes <= 0 || len(q.items) >= maxFrames {
		return false
	}
	payload := event.Frame.Payload()
	if len(payload) > maxBytes-q.bytes {
		return false
	}
	q.items = append(q.items, responsesWSQueuedCreate{payload: payload, receivedAt: event.ReceivedAt})
	q.bytes += len(payload)
	return true
}

func (q *responsesWSCreateQueue) Pop() (responsesWSQueuedCreate, bool) {
	if q == nil || len(q.items) == 0 {
		return responsesWSQueuedCreate{}, false
	}
	item := q.items[0]
	q.items[0] = responsesWSQueuedCreate{}
	q.items = q.items[1:]
	q.bytes -= len(item.payload)
	if q.bytes < 0 {
		q.bytes = 0
	}
	return item, true
}

func (q *responsesWSCreateQueue) Clear() {
	if q == nil {
		return
	}
	for i := range q.items {
		q.items[i] = responsesWSQueuedCreate{}
	}
	q.items = nil
	q.bytes = 0
}

type responsesWSInjectState struct {
	pending        int
	pendingTargets map[string]int
	terminalSeen   bool
	deferred       []responsesws.Frame
	deferredBytes  int
	sendContext    context.Context
	cancelSend     context.CancelFunc
}

func (s *responsesWSInjectState) AddPending(payload []byte) {
	if s == nil {
		return
	}
	s.pending++
	responseID := strings.TrimSpace(responsesWSPayloadResponseID(payload))
	if responseID == "" {
		return
	}
	if s.pendingTargets == nil {
		s.pendingTargets = make(map[string]int)
	}
	s.pendingTargets[responseID]++
}

func (s *responsesWSInjectState) CanAcknowledge(payload []byte) bool {
	if s == nil || s.pending <= 0 || !responsesWSIsProviderInjectAcknowledgement(payload) {
		return false
	}
	if len(s.pendingTargets) == 0 {
		return true
	}
	return s.pendingTargets[strings.TrimSpace(responsesWSPayloadResponseID(payload))] > 0
}

func (s *responsesWSInjectState) Acknowledge(payload []byte) bool {
	if !s.CanAcknowledge(payload) {
		return false
	}
	s.pending--
	responseID := strings.TrimSpace(responsesWSPayloadResponseID(payload))
	if count := s.pendingTargets[responseID]; count > 1 {
		s.pendingTargets[responseID] = count - 1
	} else if count == 1 {
		delete(s.pendingTargets, responseID)
	}
	return true
}

func (s *responsesWSInjectState) MarkTerminal() bool {
	if s == nil {
		return true
	}
	s.terminalSeen = true
	return s.pending == 0
}

func (s *responsesWSInjectState) TerminalBarrierComplete() bool {
	return s == nil || (s.terminalSeen && s.pending == 0)
}

func (s *responsesWSInjectState) Context(parent context.Context) context.Context {
	if s == nil {
		return parent
	}
	if s.sendContext == nil {
		if parent == nil {
			parent = context.Background()
		}
		s.sendContext, s.cancelSend = context.WithCancel(parent)
	}
	return s.sendContext
}

func (s *responsesWSInjectState) Reset() {
	if s == nil {
		return
	}
	if s.cancelSend != nil {
		s.cancelSend()
	}
	for i := range s.deferred {
		s.deferred[i] = responsesws.Frame{}
	}
	*s = responsesWSInjectState{}
}

type responsesWSTurnFinalization struct {
	attemptID string
}

type responsesWSPendingCleanup struct {
	attempt   *ResponsesWSTurnAttempt
	openingID string
	phase     responsesWSPendingTurnPhase
	provider  responsesWSPendingProviderState
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
		// Keep the triggering provider lifecycle observation before failing closed;
		// only the replay payload is intentionally dropped on overflow.
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

func (j *responsesWSProviderJournal) Project() responsesws.ProviderActivityProjection {
	if j == nil || len(j.entries) == 0 {
		return responsesws.ProviderActivityProjection{}
	}
	var out responsesws.ProviderActivityProjection
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
	s.pending = responsesWSPendingTurn{}
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
	runWG        sync.WaitGroup
	sendCommands chan responsesWSSendCommand
	sendBytes    atomic.Int64
	sendOnce     sync.Once

	setupCancelMu sync.Mutex
	setupCancel   context.CancelFunc
}

type responsesWSCloseState struct {
	workStopped         atomic.Bool
	closed              atomic.Bool
	ingressClosed       atomic.Bool
	clientClosed        atomic.Bool
	closeIntentPosted   atomic.Bool
	downstreamCloseSent atomic.Bool
	backpressurePosted  atomic.Bool

	postMu             sync.Mutex
	postedSequence     uint64
	closureCutSequence uint64
	reducingCut        bool
}

type responsesWSWatchdogState struct {
	lastActivityMu sync.Mutex
	lastActivity   time.Time

	activeTurnMu       sync.Mutex
	activeTurnTimer    *time.Timer
	activeTurnTimerGen int64
}
