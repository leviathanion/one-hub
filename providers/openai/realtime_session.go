package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"one-api/common"
	"one-api/common/config"
	"one-api/common/requester"
	runtimesession "one-api/runtime/session"
	"one-api/types"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const openAIRealtimeReadTimeout = 2 * time.Minute
const openAIRealtimeDetachGraceTimeout = 30 * time.Second
const openAIRealtimeFinalizedResponseIDLimit = 16

type openAIRealtimeOutbound struct {
	messageType int
	payload     []byte
	usage       *types.UsageEvent
	err         error
}

type openAIRealtimePendingTurn struct {
	state  *openAIRealtimeTurnState
	reason string
}

type openAIRealtimeFinalizedTurn struct {
	observer runtimesession.TurnObserver
	payload  runtimesession.TurnFinalizePayload
}

type openAIRealtimeTurnSelection struct {
	state           *openAIRealtimeTurnState
	dropAttribution bool
}

type openAIRealtimeSession struct {
	provider   *OpenAIProvider
	model      string
	sessionID  string
	conn       *websocket.Conn
	compatMode bool

	recvCh       chan openAIRealtimeOutbound
	closed       chan struct{}
	detached     chan struct{}
	closeOnce    sync.Once
	detachOnce   sync.Once
	detachLog    sync.Once
	writeMu      sync.Mutex
	detachTimer  *time.Timer
	detachMu     sync.Mutex
	mu           sync.Mutex
	detachReason string

	turnSeq             int64
	turn                *openAIRealtimeTurnState
	pendingTurns        []openAIRealtimePendingTurn
	recentFinalizedIDs  []string
	turnObserverFactory runtimesession.TurnObserverFactory
}

func (p *OpenAIProvider) OpenRealtimeSession(modelName string) (runtimesession.RealtimeSession, *types.OpenAIErrorWithStatusCode) {
	conn, errWithCode := p.openRealtimeConn(modelName)
	if errWithCode != nil {
		return nil, errWithCode
	}

	session := &openAIRealtimeSession{
		provider:   p,
		model:      strings.TrimSpace(modelName),
		sessionID:  readOpenAIRealtimeSessionID(p),
		conn:       conn,
		compatMode: config.OpenAIRealtimeSessionCompatMode,
		recvCh:     make(chan openAIRealtimeOutbound, 128),
		closed:     make(chan struct{}),
		detached:   make(chan struct{}),
	}
	session.configureConn()
	go session.readLoop()
	return session, nil
}

func (p *OpenAIProvider) openRealtimeConn(modelName string) (*websocket.Conn, *types.OpenAIErrorWithStatusCode) {
	url, errWithCode := p.GetSupportedAPIUri(config.RelayModeChatRealtime)
	if errWithCode != nil {
		return nil, errWithCode
	}

	fullRequestURL := p.GetFullRequestURL(url, modelName)
	httpHeaders := make(http.Header)
	if p.IsAzure {
		httpHeaders.Set("api-key", p.Channel.Key)
	} else {
		httpHeaders.Set("Authorization", fmt.Sprintf("Bearer %s", p.Channel.Key))
	}
	httpHeaders.Set("OpenAI-Beta", "realtime=v1")

	proxyAddr := ""
	if p.Channel != nil && p.Channel.Proxy != nil {
		proxyAddr = *p.Channel.Proxy
	}
	wsRequester := requester.NewWSRequester(proxyAddr)
	wsConn, err := wsRequester.NewRequest(fullRequestURL, httpHeaders)
	if err != nil {
		return nil, common.ErrorWrapper(err, "ws_request_failed", http.StatusInternalServerError)
	}
	return wsConn, nil
}

func readOpenAIRealtimeSessionID(p *OpenAIProvider) string {
	if p != nil && p.Context != nil && p.Context.Request != nil {
		if sessionID := runtimesession.ReadClientSessionID(p.Context.Request); sessionID != "" {
			return sessionID
		}
	}
	return uuid.NewString()
}

func (s *openAIRealtimeSession) SendClient(ctx context.Context, mt int, payload []byte) error {
	if s == nil || s.conn == nil || s.isDetached() {
		return runtimesession.ErrSessionClosed
	}

	normalizedPayload, eventType, err := normalizeOpenAIRealtimeClientPayload(payload, mt, s.model, s.compatMode)
	if err != nil {
		return err
	}
	var (
		startedTurn *openAIRealtimeTurnState
		finalizers  []openAIRealtimeFinalizedTurn
	)
	if eventType == "response.create" {
		startedTurn, finalizers, err = s.startTurn()
		if err != nil {
			return err
		}
		runOpenAIRealtimeFinalizers(finalizers)
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.conn.WriteMessage(mt, normalizedPayload); err != nil {
		if startedTurn != nil {
			s.rollbackTurn(startedTurn)
		}
		return types.NewErrorEvent("", "system_error", "ws_write_failed", err.Error())
	}
	return nil
}

func (s *openAIRealtimeSession) Recv(ctx context.Context) (int, []byte, *types.UsageEvent, error) {
	if messageType, payload, usage, err, handled := s.recvQueuedOutbound(); handled {
		return messageType, payload, usage, err
	}

	select {
	case <-ctx.Done():
		return 0, nil, nil, ctx.Err()
	case <-s.detached:
		return 0, nil, nil, runtimesession.ErrSessionClosed
	case <-s.closed:
		if messageType, payload, usage, err, handled := s.recvQueuedOutbound(); handled {
			return messageType, payload, usage, err
		}
		return 0, nil, nil, runtimesession.ErrSessionClosed
	case outbound, ok := <-s.recvCh:
		return decodeOpenAIRealtimeOutbound(outbound, ok)
	}
}

func (s *openAIRealtimeSession) Detach(reason string) {
	if s == nil {
		return
	}
	s.detachOnce.Do(func() {
		s.detachReason = strings.TrimSpace(reason)
		close(s.detached)
		s.startDetachTimer()
	})
}

func (s *openAIRealtimeSession) Abort(reason string) {
	s.close(reason)
}

func (s *openAIRealtimeSession) SupportsGracefulDetach() bool {
	return false
}

func (s *openAIRealtimeSession) SetTurnObserverFactory(factory runtimesession.TurnObserverFactory) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.turnObserverFactory = factory
	if s.turn != nil && s.turn.observer == nil && factory != nil {
		s.turn.observer = runtimesession.GuardTurnObserver(factory())
	}
}

func (s *openAIRealtimeSession) configureConn() {
	if s == nil || s.conn == nil {
		return
	}
	s.conn.SetPingHandler(func(appData string) error {
		s.writeMu.Lock()
		defer s.writeMu.Unlock()
		return s.conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(10*time.Second))
	})
	s.conn.SetPongHandler(func(string) error {
		return s.conn.SetReadDeadline(time.Now().Add(openAIRealtimeReadTimeout))
	})
}

func (s *openAIRealtimeSession) readLoop() {
	defer close(s.recvCh)
	defer s.close("upstream_closed")

	for {
		_ = s.conn.SetReadDeadline(time.Now().Add(openAIRealtimeReadTimeout))
		messageType, payload, err := s.conn.ReadMessage()
		if err != nil {
			providerErr := types.NewErrorEvent("", "provider_error", "provider_connection_closed", err.Error())
			s.enqueueOutbound(openAIRealtimeOutbound{
				messageType: websocket.TextMessage,
				payload:     []byte(providerErr.Error()),
				err:         runtimesession.ErrSessionClosed,
			})
			return
		}

		outbound, shouldClose := s.observeSupplierMessage(messageType, payload)
		if !s.enqueueOutbound(outbound) {
			return
		}
		if shouldClose {
			return
		}
	}
}

func (s *openAIRealtimeSession) observeSupplierMessage(messageType int, payload []byte) (openAIRealtimeOutbound, bool) {
	outbound := openAIRealtimeOutbound{messageType: messageType, payload: payload}
	if messageType != websocket.TextMessage {
		return outbound, false
	}

	var event types.Event
	if err := json.Unmarshal(payload, &event); err != nil {
		return outbound, false
	}
	if s.compatMode && event.IsError() {
		outbound.err = runtimesession.ErrSessionClosed
		return outbound, true
	}

	receivedAt := time.Now()
	eventType := strings.TrimSpace(event.Type)
	terminal, terminationReason := openAIRealtimeTurnTerminal(eventType, &event)
	usage := openAIRealtimeResponseUsage(event.Response).Clone()
	responseID := ""
	if event.Response != nil {
		responseID = strings.TrimSpace(event.Response.ID)
	}

	var (
		usageDelta *types.UsageEvent
		observer   runtimesession.TurnObserver
		turnState  *openAIRealtimeTurnState
	)

	s.mu.Lock()
	selection := s.selectSupplierTurnLocked(responseID)
	turnState = selection.state
	if turnState != nil {
		turnState.observeSupplierEvent(eventType, responseID, receivedAt, event.IsError())
		usageDelta = turnState.applyUsageSnapshot(usage)
		observer = turnState.observer
	} else if !selection.dropAttribution {
		usageDelta = usage.Clone()
	}
	s.mu.Unlock()

	if usageDelta != nil {
		outbound.usage = usageDelta.Clone()
		if observer != nil {
			if err := observer.ObserveTurnUsage(usageDelta.Clone()); err != nil {
				runOpenAIRealtimeFinalizers(s.finalizeObservedTurnState(turnState, "quota_exhausted", receivedAt))
				quotaErr := types.NewErrorEvent("", "system_error", "system_error", err.Error())
				outbound.err = runtimesession.NewClientPayloadError(quotaErr, []byte(quotaErr.Error()))
				return outbound, true
			}
		}
	}

	if terminal {
		if event.IsError() {
			s.releaseTurnStateForRecovery(turnState, terminationReason)
		} else if finalized := s.finalizeObservedTurnState(turnState, terminationReason, receivedAt); len(finalized) > 0 {
			runOpenAIRealtimeFinalizers(finalized)
		}
	}

	return outbound, false
}

func (s *openAIRealtimeSession) startTurn() (*openAIRealtimeTurnState, []openAIRealtimeFinalizedTurn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.turn != nil {
		return nil, nil, newOpenAIRealtimeClientError("session_busy", "realtime session already has an inflight response")
	}
	finalized := s.finalizePendingTurnsLocked("", time.Now())
	s.turnSeq++
	var observer runtimesession.TurnObserver
	if s.turnObserverFactory != nil {
		observer = runtimesession.GuardTurnObserver(s.turnObserverFactory())
	}
	s.turn = newOpenAIRealtimeTurnState(s.turnSeq, time.Now(), observer)
	return s.turn, finalized, nil
}

func (s *openAIRealtimeSession) rollbackTurn(turn *openAIRealtimeTurnState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.turn == turn {
		s.turn = nil
	}
}

func (s *openAIRealtimeSession) finalizeTurn(reason string, now time.Time) (runtimesession.TurnObserver, runtimesession.TurnFinalizePayload) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.finalizeTurnLocked(reason, now)
}

func (s *openAIRealtimeSession) finalizeTurnLocked(reason string, now time.Time) (runtimesession.TurnObserver, runtimesession.TurnFinalizePayload) {
	if s.turn == nil {
		return nil, runtimesession.TurnFinalizePayload{}
	}
	responseIDs := s.turn.responseIDs()
	observer, payload := s.turn.finalize(s.sessionID, s.model, reason, now)
	s.rememberFinalizedResponseIDsLocked(append(responseIDs, payload.LastResponseID)...)
	s.turn = nil
	return observer, payload
}

func (s *openAIRealtimeSession) close(reason string) {
	var finalized []openAIRealtimeFinalizedTurn
	s.closeOnce.Do(func() {
		now := time.Now()
		if finalizeObserver, finalizePayload := s.finalizeTurn(strings.TrimSpace(reason), now); finalizeObserver != nil {
			finalized = append(finalized, openAIRealtimeFinalizedTurn{
				observer: finalizeObserver,
				payload:  finalizePayload,
			})
		}
		finalized = append(finalized, s.finalizePendingTurns(strings.TrimSpace(reason), now)...)
		s.stopDetachTimer()
		if s.conn != nil {
			_ = s.conn.Close()
		}
		close(s.closed)
	})
	runOpenAIRealtimeFinalizers(finalized)
}

func (s *openAIRealtimeSession) isDetached() bool {
	if s == nil {
		return true
	}
	select {
	case <-s.detached:
		return true
	default:
		return false
	}
}

func (s *openAIRealtimeSession) recvQueuedOutbound() (int, []byte, *types.UsageEvent, error, bool) {
	if s == nil {
		return 0, nil, nil, runtimesession.ErrSessionClosed, true
	}
	select {
	case outbound, ok := <-s.recvCh:
		messageType, payload, usage, err := decodeOpenAIRealtimeOutbound(outbound, ok)
		return messageType, payload, usage, err, true
	default:
		return 0, nil, nil, nil, false
	}
}

func decodeOpenAIRealtimeOutbound(outbound openAIRealtimeOutbound, ok bool) (int, []byte, *types.UsageEvent, error) {
	if !ok {
		return 0, nil, nil, runtimesession.ErrSessionClosed
	}
	return outbound.messageType, outbound.payload, outbound.usage, outbound.err
}

func runOpenAIRealtimeFinalizers(finalized []openAIRealtimeFinalizedTurn) {
	for _, current := range finalized {
		if current.observer != nil {
			current.observer.FinalizeTurn(current.payload)
		}
	}
}

func openAIRealtimeTurnTerminal(eventType string, event *types.Event) (bool, string) {
	if event != nil && event.IsError() {
		return true, types.EventTypeError
	}
	if strings.TrimSpace(eventType) == types.EventTypeResponseDone {
		return true, types.EventTypeResponseDone
	}
	return false, ""
}

func (s *openAIRealtimeSession) selectSupplierTurnLocked(responseID string) openAIRealtimeTurnSelection {
	responseID = strings.TrimSpace(responseID)
	if responseID == "" {
		if s.turn != nil {
			return openAIRealtimeTurnSelection{state: s.turn}
		}
		if len(s.pendingTurns) > 0 {
			return openAIRealtimeTurnSelection{state: s.pendingTurns[0].state}
		}
		return openAIRealtimeTurnSelection{}
	}

	if s.turn != nil && s.turn.matchesResponseID(responseID) {
		return openAIRealtimeTurnSelection{state: s.turn}
	}
	for _, pending := range s.pendingTurns {
		if pending.state != nil && pending.state.matchesResponseID(responseID) {
			return openAIRealtimeTurnSelection{state: pending.state}
		}
	}
	if s.isRecentlyFinalizedResponseIDLocked(responseID) {
		return openAIRealtimeTurnSelection{dropAttribution: true}
	}
	if s.turn != nil {
		return openAIRealtimeTurnSelection{state: s.turn}
	}
	if len(s.pendingTurns) > 0 {
		return openAIRealtimeTurnSelection{state: s.pendingTurns[0].state}
	}
	return openAIRealtimeTurnSelection{}
}

func (s *openAIRealtimeSession) releaseTurnStateForRecovery(turnState *openAIRealtimeTurnState, reason string) {
	if s == nil || turnState == nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.turn == turnState {
		s.pendingTurns = append(s.pendingTurns, openAIRealtimePendingTurn{
			state:  s.turn,
			reason: strings.TrimSpace(reason),
		})
		s.turn = nil
		return
	}

	if index := s.pendingTurnIndexLocked(turnState); index >= 0 && strings.TrimSpace(reason) != "" {
		s.pendingTurns[index].reason = strings.TrimSpace(reason)
	}
}

func (s *openAIRealtimeSession) finalizeObservedTurnState(turnState *openAIRealtimeTurnState, reason string, now time.Time) []openAIRealtimeFinalizedTurn {
	if s == nil || turnState == nil {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.turn == turnState {
		observer, payload := s.finalizeTurnLocked(reason, now)
		if observer == nil {
			return nil
		}
		return []openAIRealtimeFinalizedTurn{{
			observer: observer,
			payload:  payload,
		}}
	}

	index := s.pendingTurnIndexLocked(turnState)
	if index < 0 {
		return nil
	}

	pending := s.pendingTurns[index]
	s.pendingTurns = append(append([]openAIRealtimePendingTurn(nil), s.pendingTurns[:index]...), s.pendingTurns[index+1:]...)
	return s.finalizePendingTurn(pending, reason, now)
}

func (s *openAIRealtimeSession) pendingTurnIndexLocked(turnState *openAIRealtimeTurnState) int {
	for index, pending := range s.pendingTurns {
		if pending.state == turnState {
			return index
		}
	}
	return -1
}

func (s *openAIRealtimeSession) finalizePendingTurnsLocked(defaultReason string, now time.Time) []openAIRealtimeFinalizedTurn {
	if len(s.pendingTurns) == 0 {
		return nil
	}
	finalized := make([]openAIRealtimeFinalizedTurn, 0, len(s.pendingTurns))
	for _, pending := range s.pendingTurns {
		finalized = append(finalized, s.finalizePendingTurn(pending, defaultReason, now)...)
	}
	s.pendingTurns = nil
	return finalized
}

func (s *openAIRealtimeSession) finalizePendingTurns(defaultReason string, now time.Time) []openAIRealtimeFinalizedTurn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.finalizePendingTurnsLocked(defaultReason, now)
}

func (s *openAIRealtimeSession) finalizePendingTurn(pending openAIRealtimePendingTurn, defaultReason string, now time.Time) []openAIRealtimeFinalizedTurn {
	if pending.state == nil {
		return nil
	}
	reason := strings.TrimSpace(pending.reason)
	if reason == "" {
		reason = strings.TrimSpace(defaultReason)
	}
	responseIDs := pending.state.responseIDs()
	observer, payload := pending.state.finalize(s.sessionID, s.model, reason, now)
	s.rememberFinalizedResponseIDsLocked(append(responseIDs, payload.LastResponseID)...)
	if observer == nil {
		return nil
	}
	return []openAIRealtimeFinalizedTurn{{
		observer: observer,
		payload:  payload,
	}}
}

func (s *openAIRealtimeSession) enqueueOutbound(outbound openAIRealtimeOutbound) bool {
	if s == nil {
		return false
	}

	select {
	case <-s.closed:
		return false
	default:
	}
	select {
	case <-s.detached:
		return s.discardDetachedOutbound()
	default:
	}

	select {
	case <-s.closed:
		return false
	case <-s.detached:
		return s.discardDetachedOutbound()
	case s.recvCh <- outbound:
		return true
	}
}

func (s *openAIRealtimeSession) discardDetachedOutbound() bool {
	if s == nil {
		return false
	}

	s.detachLog.Do(func() {
		reason := strings.TrimSpace(s.detachReason)
		if reason == "" {
			reason = "detached"
		}
		log.Printf(
			"dropping detached realtime outbound events while draining upstream for up to %s (reason=%s)",
			openAIRealtimeDetachGraceTimeout,
			reason,
		)
	})
	return true
}

func (s *openAIRealtimeSession) startDetachTimer() {
	if s == nil || openAIRealtimeDetachGraceTimeout <= 0 {
		return
	}
	s.detachMu.Lock()
	defer s.detachMu.Unlock()
	if s.detachTimer != nil {
		return
	}
	s.detachTimer = time.AfterFunc(openAIRealtimeDetachGraceTimeout, func() {
		s.Abort("detach_timeout")
	})
}

func (s *openAIRealtimeSession) stopDetachTimer() {
	if s == nil {
		return
	}
	s.detachMu.Lock()
	defer s.detachMu.Unlock()
	if s.detachTimer == nil {
		return
	}
	s.detachTimer.Stop()
	s.detachTimer = nil
}

func normalizeOpenAIRealtimeClientPayload(payload []byte, messageType int, modelName string, compatMode bool) ([]byte, string, error) {
	if messageType != websocket.TextMessage {
		return payload, "", nil
	}

	var message map[string]any
	if err := json.Unmarshal(payload, &message); err != nil {
		return payload, "", nil
	}

	eventType, _ := message["type"].(string)
	eventType = strings.TrimSpace(eventType)
	if compatMode {
		return payload, eventType, nil
	}
	if eventType != "response.create" {
		return payload, eventType, nil
	}

	trimmedModel := strings.TrimSpace(modelName)
	if trimmedModel == "" {
		return payload, eventType, nil
	}

	if response, ok := message["response"].(map[string]any); ok {
		if strings.TrimSpace(anyToString(response["model"])) == "" {
			response["model"] = trimmedModel
			message["response"] = response
		}
	} else if strings.TrimSpace(anyToString(message["model"])) == "" {
		message["model"] = trimmedModel
	}

	normalized, err := json.Marshal(message)
	if err != nil {
		return payload, eventType, nil
	}
	return normalized, eventType, nil
}

func (s *openAIRealtimeSession) rememberFinalizedResponseIDsLocked(responseIDs ...string) {
	if len(responseIDs) == 0 {
		return
	}

	updated := make([]string, 0, min(len(s.recentFinalizedIDs)+len(responseIDs), openAIRealtimeFinalizedResponseIDLimit))
	for _, existing := range s.recentFinalizedIDs {
		if strings.TrimSpace(existing) != "" {
			updated = append(updated, existing)
		}
	}
	for _, responseID := range responseIDs {
		trimmed := strings.TrimSpace(responseID)
		if trimmed == "" {
			continue
		}
		filtered := updated[:0]
		for _, existing := range updated {
			if existing != trimmed {
				filtered = append(filtered, existing)
			}
		}
		updated = append(filtered, trimmed)
	}
	if len(updated) > openAIRealtimeFinalizedResponseIDLimit {
		updated = append([]string(nil), updated[len(updated)-openAIRealtimeFinalizedResponseIDLimit:]...)
	}
	s.recentFinalizedIDs = updated
}

func (s *openAIRealtimeSession) isRecentlyFinalizedResponseIDLocked(responseID string) bool {
	responseID = strings.TrimSpace(responseID)
	if responseID == "" {
		return false
	}
	for _, existing := range s.recentFinalizedIDs {
		if existing == responseID {
			return true
		}
	}
	return false
}

func anyToString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	default:
		return ""
	}
}

func openAIRealtimeResponseUsage(response *types.ResponseEvent) *types.UsageEvent {
	if response == nil || response.Usage == nil {
		return nil
	}
	return response.Usage
}
