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

// opening/pending describe local setup only; work evidence lives in observation.
type responsesWSTurnSlots struct {
	opening responsesWSOpeningTurn
	pending responsesWSPendingTurn
	queue   responsesWSCreateQueue
	history responsesWSTurnHistory
}
type responsesWSTurnHistory struct {
	localEphemeralResponseIDs []string
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
	attempt   *ResponsesWSTurnAttempt
}
type responsesWSPendingCleanup struct {
	attempt   *ResponsesWSTurnAttempt
	openingID string
	phase     responsesWSPendingTurnPhase
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

func (s *responsesWSTurnSlots) BeginOpening(opening responsesWSOpeningTurn) error {
	if s == nil {
		return errors.New("turn slots are required")
	}
	opening.openingID = strings.TrimSpace(opening.openingID)
	if opening.openingID == "" {
		return errors.New("opening id is required")
	}
	s.opening = opening
	s.pending = responsesWSPendingTurn{phase: responsesWSPendingTurnOpening, openingID: opening.openingID}
	return nil
}
func (s *responsesWSTurnSlots) ClearPending() responsesWSPendingCleanup {
	if s == nil {
		return responsesWSPendingCleanup{}
	}
	cleanup := responsesWSPendingCleanup{attempt: s.pending.attempt, openingID: s.pending.openingID, phase: s.pending.phase}
	s.pending = responsesWSPendingTurn{}
	return cleanup
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

	postMu      sync.Mutex
	reducingCut bool
}

type responsesWSWatchdogState struct {
	lastActivityMu sync.Mutex
	lastActivity   time.Time
}
