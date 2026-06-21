package responsesws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"one-api/common/logger"
	"one-api/common/requester"
	"one-api/types"
)

const defaultBridgeRecvQueueSize = 128
const defaultBridgeDrainTimeout = 250 * time.Millisecond
const defaultBridgeMaxStreamEventBytes int64 = 1 << 20

var ErrBridgeBusy = errors.New("responses websocket bridge stream already active")
var ErrBridgeOpenCancelled = errors.New("responses websocket bridge stream opening cancelled")
var ErrBridgeOpenTimeout = errors.New("responses websocket bridge stream opening timed out")
var ErrInvalidBridgeStreamPayload = errors.New("responses websocket bridge stream data is not valid JSON")

// BridgeStreamOpener opens a provider HTTP Responses stream for the bridge.
type BridgeStreamOpener interface {
	// OpenBridgeStream must observe ctx cancellation. BridgeSession open timeout
	// returns to the caller without waiting for a non-cooperative opener; if a
	// stream is returned after timeout, BridgeSession closes it immediately.
	OpenBridgeStream(ctx context.Context, req BridgeStreamRequest) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode, error)
}

type BridgeStreamRequest struct {
	Frame                     Frame
	DefaultPreviousResponseID string
}

type bridgePreviousResponseDefaultRule struct {
	Value          string
	ConsumeInitial bool
}

type bridgeStreamOnce struct {
	requester.StreamReaderInterface[string]
	closeOnce sync.Once
}

func newBridgeStreamOnce(stream requester.StreamReaderInterface[string]) requester.StreamReaderInterface[string] {
	if stream == nil {
		return nil
	}
	return &bridgeStreamOnce{StreamReaderInterface: stream}
}

func (s *bridgeStreamOnce) Close() {
	if s == nil || s.StreamReaderInterface == nil {
		return
	}
	s.closeOnce.Do(func() {
		s.StreamReaderInterface.Close()
	})
}

// BridgeSessionOptions configures the HTTP stream bridge transport.
type BridgeSessionOptions struct {
	Context                   context.Context
	RecvQueueSize             int
	Diagnostics               BridgeDiagnosticHook
	ProviderName              string
	ChannelID                 int
	Transport                 string
	InitialPreviousResponseID string
	OpenTimeout               time.Duration
	MaxStreamEventBytes       int64
}

// BridgeSession adapts a provider Responses HTTP stream to the temporary
// ResponsesWS upstream contract. Trade-offs versus native WS are explicit:
// there is no real upstream connection-local WS state or provider close/control
// frame; response.cancel can only close the HTTP stream and emit a proxy-local
// synthetic cancellation; typed request construction may not preserve every
// unknown raw field unless the provider bridge opener does so;
// previous_response_id, prompt cache and provider-side session-cache evidence is
// weaker than a native provider WebSocket conversation.
type BridgeSession struct {
	opener       BridgeStreamOpener
	base         context.Context
	diag         BridgeDiagnosticHook
	providerName string
	channelID    int
	transport    string
	openTimeout  time.Duration
	maxEventSize int64

	recvCh chan UpstreamEvent
	done   chan struct{}

	mu                          sync.Mutex
	active                      requester.StreamReaderInterface[string]
	activeCancel                context.CancelFunc
	activeAttemptID             string
	activeStreamID              uint64
	nextStreamID                uint64
	cancelledStreamIDs          map[uint64]struct{}
	backpressureCancelStreamIDs map[uint64]struct{}
	finishedStreamIDs           map[uint64]struct{}
	opening                     bool
	openingCancel               context.CancelFunc
	openingAttemptID            string
	initialPreviousResponseID   string
	closed                      bool
}

func NewBridgeSession(opener BridgeStreamOpener, options BridgeSessionOptions) *BridgeSession {
	queueSize := options.RecvQueueSize
	if queueSize <= 0 {
		queueSize = defaultBridgeRecvQueueSize
	}
	base := options.Context
	if base == nil {
		base = context.Background()
	}
	maxEventSize := options.MaxStreamEventBytes
	if maxEventSize <= 0 {
		maxEventSize = defaultBridgeMaxStreamEventBytes
	}
	return &BridgeSession{
		opener:                    opener,
		base:                      base,
		diag:                      options.Diagnostics,
		providerName:              options.ProviderName,
		channelID:                 options.ChannelID,
		transport:                 options.Transport,
		openTimeout:               options.OpenTimeout,
		maxEventSize:              maxEventSize,
		recvCh:                    make(chan UpstreamEvent, queueSize),
		done:                      make(chan struct{}),
		initialPreviousResponseID: strings.TrimSpace(options.InitialPreviousResponseID),
	}
}

func (s *BridgeSession) SupportsBridgeContinuationDefault() bool {
	return s != nil
}

func (s *BridgeSession) SendClientWithResult(ctx context.Context, req SendRequest) ResponsesWSTransportSendResult {
	return s.sendClient(ctx, req)
}

func (s *BridgeSession) SendControl(ctx context.Context, req SendRequest) ResponsesWSTransportSendResult {
	if s == nil {
		return ResponsesWSTransportSendResult{Status: ResponsesWSTransportSendNotAttempted, Err: ErrUpstreamClosed}
	}
	if ctx == nil {
		ctx = s.base
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return ResponsesWSTransportSendResult{Status: ResponsesWSTransportSendNotAttempted, Err: err}
	}
	frame := req.Frame
	if frame.Kind() != FrameKindText || frame.PayloadLen() == 0 {
		return ResponsesWSTransportSendResult{Status: ResponsesWSTransportSendNotAttempted, Err: ErrInvalidFrame}
	}
	if err := validateClientAttemptID(req); err != nil {
		return ResponsesWSTransportSendResult{Status: ResponsesWSTransportSendNotAttempted, Err: err}
	}
	if bridgeClientEventType(frame.Payload()) != "response.cancel" {
		return ResponsesWSTransportSendResult{Status: ResponsesWSTransportSendNotAttempted, Err: ErrInvalidFrame}
	}
	return s.cancelActiveStream(req.AttemptID)
}

func (s *BridgeSession) sendClient(ctx context.Context, req SendRequest) ResponsesWSTransportSendResult {
	if s == nil || s.opener == nil {
		return ResponsesWSTransportSendResult{Status: ResponsesWSTransportSendNotAttempted, Err: ErrUpstreamClosed}
	}
	if ctx == nil {
		ctx = s.base
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return ResponsesWSTransportSendResult{Status: ResponsesWSTransportSendNotAttempted, Err: err}
	}
	frame := req.Frame
	if frame.Kind() != FrameKindText || frame.PayloadLen() == 0 {
		return ResponsesWSTransportSendResult{Status: ResponsesWSTransportSendNotAttempted, Err: ErrInvalidFrame}
	}
	if err := validateClientAttemptID(req); err != nil {
		return ResponsesWSTransportSendResult{Status: ResponsesWSTransportSendNotAttempted, Err: err}
	}
	eventType := bridgeClientEventType(frame.Payload())
	select {
	case <-s.done:
		return ResponsesWSTransportSendResult{Status: ResponsesWSTransportSendNotAttempted, Err: ErrUpstreamClosed}
	default:
	}

	if eventType == "response.cancel" {
		return s.cancelActiveStream(req.AttemptID)
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ResponsesWSTransportSendResult{Status: ResponsesWSTransportSendNotAttempted, Err: ErrUpstreamClosed}
	}
	if s.active != nil || s.opening {
		s.mu.Unlock()
		return ResponsesWSTransportSendResult{Status: ResponsesWSTransportSendNotAttempted, Err: ErrBridgeBusy}
	}
	previousResponseIDDefault := s.previousResponseDefaultRuleLocked(req)
	openCtx, cancelOpen := context.WithCancel(ctx)
	attemptID := strings.TrimSpace(req.AttemptID)
	s.opening = true
	s.openingCancel = cancelOpen
	s.openingAttemptID = attemptID
	s.mu.Unlock()

	stream, openErr, prepareErr := s.openBridgeStreamWithTimeout(openCtx, cancelOpen, BridgeStreamRequest{
		Frame:                     frame,
		DefaultPreviousResponseID: previousResponseIDDefault.Value,
	})
	s.mu.Lock()
	openingCancelled := !s.opening || s.openingCancel == nil
	if !openingCancelled {
		s.opening = false
		s.openingCancel = nil
		s.openingAttemptID = ""
	}
	s.mu.Unlock()
	if openingCancelled {
		cancelOpen()
		if stream != nil {
			stream.Close()
		}
		if prepareErr != nil {
			return ResponsesWSTransportSendResult{Status: ResponsesWSTransportSendNotAttempted, Err: prepareErr}
		}
		s.consumeInitialPreviousResponseID(previousResponseIDDefault.ConsumeInitial)
		return ResponsesWSTransportSendResult{Status: ResponsesWSTransportSendAmbiguous, Err: ErrBridgeOpenCancelled}
	}
	if prepareErr != nil {
		cancelOpen()
		if stream != nil {
			stream.Close()
		}
		if errors.Is(prepareErr, ErrAdapterPanic) {
			if !s.enqueueBridgeResultOrAbort(BridgeEventResult{
				Err:         prepareErr,
				CloseStream: true,
				Origin:      RecvDetailOriginAdapterPanic,
			}, attemptID, "bridge_open_panic_queue_full", RecvDetailPhasePrepareClientFrame) {
				return ResponsesWSTransportSendResult{Status: ResponsesWSTransportSendAmbiguous, Err: ErrNativeQueueFull}
			}
			s.Abort("bridge_open_adapter_panic")
		}
		return ResponsesWSTransportSendResult{Status: ResponsesWSTransportSendNotAttempted, Err: prepareErr}
	}
	if openErr != nil {
		s.consumeInitialPreviousResponseID(previousResponseIDDefault.ConsumeInitial)
		cancelOpen()
		if stream != nil {
			stream.Close()
		}
		if openErr.LocalError {
			err := NewBridgeOpenLocalErrorFromOpenAIError(openErr)
			if !s.enqueueBridgeResultOrAbort(BridgeEventResult{
				Err:         err,
				CloseStream: true,
				Origin:      RecvDetailOriginBridgeStreamError,
			}, attemptID, "bridge_open_local_error_enqueue_failed") {
				return ResponsesWSTransportSendResult{Status: ResponsesWSTransportSendAmbiguous, Err: ErrNativeQueueFull}
			}
			s.Abort("bridge_open_local_error")
			return ResponsesWSTransportSendResult{Status: ResponsesWSTransportSendAmbiguous, Err: openErr}
		}
		err := NewBridgeOpenProviderErrorFromOpenAIError(openErr)
		if !s.enqueueBridgeResultOrAbort(BridgeEventResult{
			Err:         err,
			CloseStream: true,
			Origin:      RecvDetailOriginBridgeOpenProviderError,
		}, attemptID, "bridge_open_provider_error_enqueue_failed") {
			return ResponsesWSTransportSendResult{Status: ResponsesWSTransportSendAmbiguous, Err: ErrNativeQueueFull}
		}
		return ResponsesWSTransportSendResult{Status: ResponsesWSTransportSendRejectedBeforeStream}
	}
	if stream == nil {
		cancelOpen()
		return ResponsesWSTransportSendResult{Status: ResponsesWSTransportSendNotAttempted, Err: ErrUpstreamClosed}
	}
	s.consumeInitialPreviousResponseID(previousResponseIDDefault.ConsumeInitial)
	s.mu.Lock()
	if s.closed || s.active != nil {
		s.mu.Unlock()
		cancelOpen()
		stream.Close()
		return ResponsesWSTransportSendResult{Status: ResponsesWSTransportSendAmbiguous, Err: ErrBridgeBusy}
	}
	s.nextStreamID++
	streamID := s.nextStreamID
	activeStream := newBridgeStreamOnce(stream)
	// BridgeSession owns multiple close paths for an accepted active stream:
	// client cancel, Abort, queue-full cleanup, and pump defer. Even if current
	// requester streams are idempotent, the transport boundary keeps a once
	// wrapper so a provider stream is not forced to rely on that internal detail.
	s.active = activeStream
	s.activeCancel = cancelOpen
	s.activeAttemptID = attemptID
	s.activeStreamID = streamID
	s.mu.Unlock()
	if !s.enqueueBridgeResultOrAbort(BridgeEventResult{
		Origin: RecvDetailOriginBridgeStreamOpened,
	}, attemptID, "bridge_stream_opened_enqueue_failed") {
		return ResponsesWSTransportSendResult{Status: ResponsesWSTransportSendAmbiguous, Err: ErrNativeQueueFull}
	}
	go s.pump(activeStream, attemptID, streamID)
	return ResponsesWSTransportSendResult{Status: ResponsesWSTransportSendAttempted}
}

func (s *BridgeSession) previousResponseDefaultRuleLocked(req SendRequest) bridgePreviousResponseDefaultRule {
	if s == nil {
		return bridgePreviousResponseDefaultRule{}
	}
	initial := strings.TrimSpace(s.initialPreviousResponseID)
	rule := bridgePreviousResponseDefaultRule{ConsumeInitial: initial != ""}
	if dynamic := strings.TrimSpace(req.DefaultPreviousResponseID); dynamic != "" {
		rule.Value = dynamic
		return rule
	}
	rule.Value = initial
	return rule
}

func (s *BridgeSession) consumeInitialPreviousResponseID(consume bool) {
	if s == nil || !consume {
		return
	}
	s.mu.Lock()
	s.initialPreviousResponseID = ""
	s.mu.Unlock()
}

func (s *BridgeSession) cancelActiveStream(targetAttemptID string) ResponsesWSTransportSendResult {
	targetAttemptID = strings.TrimSpace(targetAttemptID)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ResponsesWSTransportSendResult{Status: ResponsesWSTransportSendNotAttempted, Err: ErrUpstreamClosed}
	}
	active := s.active
	activeCancel := s.activeCancel
	activeAttemptID := s.activeAttemptID
	activeStreamID := s.activeStreamID
	openingCancel := s.openingCancel
	if active == nil {
		if openingCancel != nil {
			if targetAttemptID == "" || s.openingAttemptID != targetAttemptID {
				s.mu.Unlock()
				return ResponsesWSTransportSendResult{
					Status: ResponsesWSTransportSendNotAttempted,
					Reason: ResponsesWSTransportSendReasonStaleBridgeCancel,
				}
			}
			s.opening = false
			s.openingCancel = nil
			s.openingAttemptID = ""
			s.mu.Unlock()
			openingCancel()
			return ResponsesWSTransportSendResult{Status: ResponsesWSTransportSendAttempted}
		}
		s.mu.Unlock()
		return ResponsesWSTransportSendResult{
			Status: ResponsesWSTransportSendNotAttempted,
			Reason: ResponsesWSTransportSendReasonNoActiveBridgeCancel,
		}
	}
	if targetAttemptID == "" || activeAttemptID != targetAttemptID {
		s.mu.Unlock()
		return ResponsesWSTransportSendResult{
			Status: ResponsesWSTransportSendNotAttempted,
			Reason: ResponsesWSTransportSendReasonStaleBridgeCancel,
		}
	}
	frame := NewTextFrame([]byte(`{"type":"response.cancelled","response":{"status":"cancelled"}}`))
	if !s.enqueueLocked(UpstreamEvent{
		Frame:        &frame,
		AttemptID:    activeAttemptID,
		DetailOrigin: RecvDetailOriginSyntheticBridge,
	}) {
		if activeStreamID != 0 {
			if s.cancelledStreamIDs == nil {
				s.cancelledStreamIDs = make(map[uint64]struct{})
			}
			s.cancelledStreamIDs[activeStreamID] = struct{}{}
			if s.backpressureCancelStreamIDs == nil {
				s.backpressureCancelStreamIDs = make(map[uint64]struct{})
			}
			s.backpressureCancelStreamIDs[activeStreamID] = struct{}{}
		}
		s.active = nil
		s.activeCancel = nil
		s.activeAttemptID = ""
		s.activeStreamID = 0
		s.opening = false
		s.openingCancel = nil
		s.openingAttemptID = ""
		s.mu.Unlock()
		if activeCancel != nil {
			activeCancel()
		}
		if openingCancel != nil {
			openingCancel()
		}
		active.Close()
		s.scheduleBackpressureCancelFallback(frame, activeAttemptID, activeStreamID)
		return ResponsesWSTransportSendResult{Status: ResponsesWSTransportSendAttempted}
	}
	if activeStreamID != 0 {
		if s.cancelledStreamIDs == nil {
			s.cancelledStreamIDs = make(map[uint64]struct{})
		}
		s.cancelledStreamIDs[activeStreamID] = struct{}{}
	}
	s.active = nil
	s.activeCancel = nil
	s.activeAttemptID = ""
	s.activeStreamID = 0
	s.opening = false
	s.openingCancel = nil
	s.openingAttemptID = ""
	s.mu.Unlock()
	if activeCancel != nil {
		activeCancel()
	}
	if openingCancel != nil {
		openingCancel()
	}
	active.Close()
	s.scheduleExpectedCancelEOF(active, activeAttemptID, activeStreamID)
	return ResponsesWSTransportSendResult{Status: ResponsesWSTransportSendAttempted}
}

func (s *BridgeSession) scheduleBackpressureCancelFallback(frame Frame, attemptID string, streamID uint64) {
	if s == nil || streamID == 0 {
		return
	}
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				s.emitDiagnostic("bridge_backpressure_cancel_fallback_panic", RecvDetailPhaseHandleProviderFrame, recovered)
			}
		}()
		timer := time.NewTimer(defaultBridgeDrainTimeout)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-s.done:
			return
		}
		if !s.consumeBackpressureCancelled(streamID) {
			return
		}
		if !s.markStreamFinished(streamID) {
			return
		}
		if !s.enqueueBlocking(UpstreamEvent{
			Frame:        &frame,
			AttemptID:    attemptID,
			DetailOrigin: RecvDetailOriginSyntheticBridge,
		}) {
			return
		}
		_ = s.enqueueBlocking(UpstreamEvent{
			AttemptID:    attemptID,
			DetailOrigin: RecvDetailOriginBridgeStreamEOF,
		})
	}()
}

type bridgeOpenResult struct {
	stream      requester.StreamReaderInterface[string]
	providerErr *types.OpenAIErrorWithStatusCode
	err         error
}

func (s *BridgeSession) openBridgeStream(ctx context.Context, req BridgeStreamRequest) (stream requester.StreamReaderInterface[string], providerErr *types.OpenAIErrorWithStatusCode, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			s.emitDiagnostic("adapter_panic", RecvDetailPhasePrepareClientFrame, recovered)
			stream = nil
			providerErr = nil
			err = ErrAdapterPanic
		}
	}()
	return s.opener.OpenBridgeStream(ctx, req)
}

func (s *BridgeSession) openBridgeStreamWithTimeout(ctx context.Context, cancelOpen context.CancelFunc, req BridgeStreamRequest) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode, error) {
	if s == nil || s.openTimeout <= 0 {
		return s.openBridgeStream(ctx, req)
	}

	// Keep resultCh unbuffered. After timeout there is no receiver, so a late
	// opener cannot select the send case and must close any returned stream.
	resultCh := make(chan bridgeOpenResult)
	timedOut := make(chan struct{})
	go func() {
		stream, providerErr, err := s.openBridgeStream(ctx, req)
		result := bridgeOpenResult{stream: stream, providerErr: providerErr, err: err}
		select {
		case resultCh <- result:
		case <-timedOut:
			if result.stream != nil {
				result.stream.Close()
			}
		}
	}()

	timer := time.NewTimer(s.openTimeout)
	defer timer.Stop()

	select {
	case result := <-resultCh:
		return result.stream, result.providerErr, result.err
	case <-timer.C:
		if cancelOpen != nil {
			cancelOpen()
		}
		close(timedOut)
		return nil, bridgeOpenTimeoutOpenAIError(), nil
	}
}

func bridgeOpenTimeoutOpenAIError() *types.OpenAIErrorWithStatusCode {
	return &types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{
			Type:    "one_hub_error",
			Code:    "ws_request_failed",
			Message: ErrBridgeOpenTimeout.Error(),
		},
		StatusCode: http.StatusGatewayTimeout,
		LocalError: true,
	}
}

func (s *BridgeSession) Recv(ctx context.Context) (UpstreamEvent, error) {
	if s == nil {
		return UpstreamEvent{}, ErrUpstreamClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case event, ok := <-s.recvCh:
		if !ok {
			return UpstreamEvent{}, ErrUpstreamClosed
		}
		return event, nil
	case <-ctx.Done():
		return UpstreamEvent{}, ctx.Err()
	case <-s.done:
		select {
		case event, ok := <-s.recvCh:
			if ok {
				return event, nil
			}
		default:
		}
		return UpstreamEvent{}, ErrUpstreamClosed
	}
}

func (s *BridgeSession) Abort(reason string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	active := s.active
	activeCancel := s.activeCancel
	openingCancel := s.openingCancel
	s.active = nil
	s.activeCancel = nil
	s.activeAttemptID = ""
	s.activeStreamID = 0
	s.opening = false
	s.openingCancel = nil
	s.openingAttemptID = ""
	close(s.done)
	s.mu.Unlock()
	if activeCancel != nil {
		activeCancel()
	}
	if openingCancel != nil {
		openingCancel()
	}
	if active != nil {
		active.Close()
	}
}

func (s *BridgeSession) pump(stream requester.StreamReaderInterface[string], attemptID string, streamID uint64) {
	var dataCh <-chan string
	var errCh <-chan error
	assembler := newBridgeSSEAssembler(s.maxEventSize)
	drained := false
	closeAndDrainOnce := func() {
		if drained {
			return
		}
		drained = true
		s.closeAndDrain(stream, dataCh, errCh)
		dataCh = nil
		errCh = nil
	}
	defer s.forgetStreamID(streamID)
	defer closeAndDrainOnce()
	defer func() {
		if recovered := recover(); recovered != nil {
			s.emitDiagnostic("adapter_panic", RecvDetailPhaseHandleProviderFrame, recovered)
			if !s.finishStreamAndEnqueueEventOrAbort(stream, streamID, UpstreamEvent{
				AttemptID:    attemptID,
				DetailOrigin: RecvDetailOriginAdapterPanic,
				DetailPhase:  RecvDetailPhaseHandleProviderFrame,
				Err:          ErrAdapterPanic,
			}, "bridge_pump_panic_queue_full") {
				return
			}
		}
	}()
	dataCh, errCh = stream.Recv()
	for {
		select {
		case raw, ok := <-dataCh:
			if !ok {
				dataCh = nil
				break
			}
			if s.streamIsFinished(streamID) {
				return
			}
			events, payloadErr := assembler.Consume(raw)
			if payloadErr != nil {
				if !s.finishStreamAndEnqueueBridgeResultOrAbort(stream, streamID, BridgeEventResult{
					Err:         payloadErr,
					CloseStream: true,
					Origin:      RecvDetailOriginBridgeStreamError,
				}, attemptID, "bridge_stream_malformed_payload_queue_full") {
					return
				}
				return
			}
			for _, event := range events {
				if !s.handleBridgeStreamData(stream, streamID, event, attemptID) {
					return
				}
			}
		case err, ok := <-errCh:
			if !ok {
				errCh = nil
				break
			}
			if s.streamIsFinished(streamID) {
				return
			}
			if s.streamWasBackpressureCancelled(streamID) && (errors.Is(err, io.EOF) || bridgeExpectedLocalCancelCloseError(err)) {
				return
			}
			origin := RecvDetailOriginBridgeStreamError
			if errors.Is(err, io.EOF) {
				if !s.flushBridgeStreamAssembler(stream, streamID, assembler, attemptID) {
					return
				}
				origin = RecvDetailOriginBridgeStreamEOF
				err = nil
			} else if s.streamWasLocallyCancelled(streamID) && bridgeExpectedLocalCancelCloseError(err) {
				if !s.flushBridgeStreamAssembler(stream, streamID, assembler, attemptID) {
					return
				}
				origin = RecvDetailOriginBridgeStreamEOF
				err = nil
			}
			if !s.finishStreamAndEnqueueBridgeResultOrAbort(stream, streamID, BridgeEventResult{
				Err:         err,
				CloseStream: true,
				Origin:      origin,
			}, attemptID, "bridge_stream_terminal_queue_full") {
				return
			}
			return
		case <-s.done:
			return
		}
		if dataCh == nil && errCh == nil {
			if s.streamWasBackpressureCancelled(streamID) {
				return
			}
			if !s.flushBridgeStreamAssembler(stream, streamID, assembler, attemptID) {
				return
			}
			if !s.finishStreamAndEnqueueBridgeResultOrAbort(stream, streamID, BridgeEventResult{
				CloseStream: true,
				Origin:      RecvDetailOriginBridgeStreamEOF,
			}, attemptID, "bridge_stream_terminal_queue_full") {
				return
			}
			return
		}
	}
}

func (s *BridgeSession) flushBridgeStreamAssembler(stream requester.StreamReaderInterface[string], streamID uint64, assembler *bridgeSSEAssembler, attemptID string) bool {
	events, err := assembler.Flush()
	if err != nil {
		return s.finishStreamAndEnqueueBridgeResultOrAbort(stream, streamID, BridgeEventResult{
			Err:         err,
			CloseStream: true,
			Origin:      RecvDetailOriginBridgeStreamError,
		}, attemptID, "bridge_stream_malformed_payload_queue_full")
	}
	for _, event := range events {
		if !s.handleBridgeStreamData(stream, streamID, event, attemptID) {
			return false
		}
	}
	return true
}

func (s *BridgeSession) handleBridgeStreamData(stream requester.StreamReaderInterface[string], streamID uint64, raw string, attemptID string) bool {
	if s.streamIsFinished(streamID) {
		return false
	}
	if bridgeStreamDone(raw) {
		result := BridgeEventResult{
			CloseStream: true,
			Origin:      RecvDetailOriginBridgeStreamEOF,
		}
		if !s.finishStreamAndEnqueueBridgeResultOrAbort(stream, streamID, result, attemptID, "bridge_stream_terminal_queue_full") {
			s.logBridgeStreamFinishNotEnqueued(streamID, attemptID, "bridge_stream_terminal_queue_full", result)
		}
		return false
	}
	payload, ok, payloadErr := bridgeStreamPayload(raw)
	if payloadErr != nil {
		result := BridgeEventResult{
			Err:         payloadErr,
			CloseStream: true,
			Origin:      RecvDetailOriginBridgeStreamError,
		}
		if !s.finishStreamAndEnqueueBridgeResultOrAbort(stream, streamID, result, attemptID, "bridge_stream_malformed_payload_queue_full") {
			s.logBridgeStreamFinishNotEnqueued(streamID, attemptID, "bridge_stream_malformed_payload_queue_full", result)
		}
		return false
	}
	if !ok {
		return true
	}
	frame := NewTextFrame(payload)
	usage := bridgeUsageFromPayload(payload)
	classified := ClassifyResponsesWSEvent(payload)
	terminal := !classified.Malformed && classified.Kind != ResponsesNonTerminal
	// A bridge terminal releases only the HTTP stream cancel target here.
	// Actor-owned attempt accounting stays serialized on the recv event.
	result := BridgeEventResult{
		EmitFrame: &frame,
		Usage:     usage,
		Origin:    RecvDetailOriginProviderStream,
	}
	if terminal {
		if !s.finishStreamAndEnqueueBridgeResultOrAbort(stream, streamID, result, attemptID, "bridge_stream_queue_full") {
			s.logBridgeStreamFinishNotEnqueued(streamID, attemptID, "bridge_stream_queue_full", result)
		}
		return false
	}
	return s.enqueueBridgeResultOrAbort(result, attemptID, "bridge_stream_queue_full")
}

func (s *BridgeSession) logBridgeStreamFinishNotEnqueued(streamID uint64, attemptID string, abortReason string, result BridgeEventResult) {
	logger.LogWarn(context.Background(), fmt.Sprintf(
		"responses websocket bridge stream finish event not enqueued: stream_id=%d attempt_id=%s abort_reason=%s origin=%s close_stream=%t err=%s",
		streamID,
		strings.TrimSpace(attemptID),
		strings.TrimSpace(abortReason),
		result.Origin,
		result.CloseStream,
		bridgeStreamFinishErrDiagnostic(result.Err),
	))
}

func bridgeStreamFinishErrDiagnostic(err error) string {
	if err == nil {
		return ""
	}
	return RedactSensitiveText(err.Error())
}

func (s *BridgeSession) closeAndDrain(stream requester.StreamReaderInterface[string], dataCh <-chan string, errCh <-chan error) {
	if stream != nil {
		stream.Close()
	}
	s.drainChannels(dataCh, errCh)
}

func (s *BridgeSession) drainChannels(dataCh <-chan string, errCh <-chan error) {
	if dataCh == nil && errCh == nil {
		return
	}
	timer := time.NewTimer(defaultBridgeDrainTimeout)
	defer timer.Stop()
	for dataCh != nil || errCh != nil {
		select {
		case _, ok := <-dataCh:
			if !ok {
				dataCh = nil
			}
		case _, ok := <-errCh:
			if !ok {
				errCh = nil
			}
		case <-timer.C:
			return
		}
	}
}

func bridgeUsageFromPayload(payload []byte) *types.UsageEvent {
	var event struct {
		EventID  string `json:"event_id"`
		Response struct {
			ID    string                `json:"id"`
			Usage *types.ResponsesUsage `json:"usage"`
		} `json:"response"`
		Usage *types.ResponsesUsage `json:"usage"`
	}
	if err := json.Unmarshal(payload, &event); err != nil {
		return nil
	}
	usage := event.Usage
	responseID := ""
	if event.Response.Usage != nil {
		usage = event.Response.Usage
		responseID = event.Response.ID
	}
	if usage == nil || usage.TotalTokens == 0 {
		return nil
	}
	return &types.UsageEvent{
		InputTokens:     usage.InputTokens,
		OutputTokens:    usage.OutputTokens,
		TotalTokens:     usage.TotalTokens,
		ProviderEventID: event.EventID,
		ResponseID:      responseID,
	}
}

func bridgeClientEventType(payload []byte) string {
	envelope, err := ParseClientEventEnvelope(payload)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(envelope.Type)
}

func (s *BridgeSession) releaseActive(stream requester.StreamReaderInterface[string]) {
	s.mu.Lock()
	if s.active == stream {
		s.active = nil
		activeCancel := s.activeCancel
		s.activeCancel = nil
		s.activeAttemptID = ""
		s.activeStreamID = 0
		s.mu.Unlock()
		if activeCancel != nil {
			activeCancel()
		}
		return
	}
	s.mu.Unlock()
}

func (s *BridgeSession) clearActive(stream requester.StreamReaderInterface[string]) {
	s.releaseActive(stream)
	stream.Close()
}

func (s *BridgeSession) streamWasLocallyCancelled(streamID uint64) bool {
	if s == nil || streamID == 0 {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.cancelledStreamIDs[streamID]
	return ok
}

func (s *BridgeSession) streamWasBackpressureCancelled(streamID uint64) bool {
	if s == nil || streamID == 0 {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.backpressureCancelStreamIDs[streamID]
	return ok
}

func (s *BridgeSession) clearBackpressureCancelled(streamID uint64) {
	if s == nil || streamID == 0 {
		return
	}
	s.mu.Lock()
	if len(s.backpressureCancelStreamIDs) > 0 {
		delete(s.backpressureCancelStreamIDs, streamID)
	}
	s.mu.Unlock()
}

func (s *BridgeSession) consumeBackpressureCancelled(streamID uint64) bool {
	if s == nil || streamID == 0 {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.backpressureCancelStreamIDs[streamID]; !ok {
		return false
	}
	delete(s.backpressureCancelStreamIDs, streamID)
	return true
}

func (s *BridgeSession) markStreamFinished(streamID uint64) bool {
	if s == nil || streamID == 0 {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.finishedStreamIDs[streamID]; ok {
		return false
	}
	if s.finishedStreamIDs == nil {
		s.finishedStreamIDs = make(map[uint64]struct{})
	}
	s.finishedStreamIDs[streamID] = struct{}{}
	return true
}

func (s *BridgeSession) streamIsFinished(streamID uint64) bool {
	if s == nil || streamID == 0 {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.finishedStreamIDs[streamID]
	return ok
}

func (s *BridgeSession) forgetStreamID(streamID uint64) {
	if s == nil || streamID == 0 {
		return
	}
	s.mu.Lock()
	if s.activeStreamID == streamID {
		s.activeStreamID = 0
	}
	if len(s.cancelledStreamIDs) > 0 {
		delete(s.cancelledStreamIDs, streamID)
	}
	if len(s.backpressureCancelStreamIDs) > 0 {
		delete(s.backpressureCancelStreamIDs, streamID)
	}
	if len(s.finishedStreamIDs) > 0 {
		delete(s.finishedStreamIDs, streamID)
	}
	s.mu.Unlock()
}

func (s *BridgeSession) scheduleExpectedCancelEOF(stream requester.StreamReaderInterface[string], attemptID string, streamID uint64) {
	if s == nil || stream == nil || streamID == 0 {
		return
	}
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				s.emitDiagnostic("bridge_expected_cancel_eof_panic", RecvDetailPhaseHandleProviderFrame, recovered)
			}
		}()
		timer := time.NewTimer(defaultBridgeDrainTimeout)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-s.done:
			return
		}
		if !s.markStreamFinished(streamID) {
			return
		}
		_ = s.releaseActiveAndEnqueueBridgeResultOrAbort(stream, BridgeEventResult{
			CloseStream: true,
			Origin:      RecvDetailOriginBridgeStreamEOF,
		}, attemptID, "bridge_cancel_expected_eof_queue_full")
	}()
}

func bridgeExpectedLocalCancelCloseError(err error) bool {
	if err == nil || errors.Is(err, requester.ErrStreamLineTooLarge) {
		return false
	}
	if errors.Is(err, context.Canceled) ||
		errors.Is(err, io.ErrClosedPipe) ||
		errors.Is(err, net.ErrClosed) {
		return true
	}
	message := strings.ToLower(strings.TrimSpace(err.Error()))
	return strings.Contains(message, "context canceled") ||
		strings.Contains(message, "closed response body") ||
		strings.Contains(message, "use of closed network connection") ||
		strings.Contains(message, "stream closed")
}

func (s *BridgeSession) emitDiagnostic(code string, phase RecvDetailPhase, recovered any) {
	if s == nil || s.diag == nil {
		return
	}
	s.diag(BridgeDiagnostic{
		Code:        strings.TrimSpace(code),
		Provider:    s.providerName,
		ChannelID:   s.channelID,
		Transport:   bridgeDiagnosticTransport(s.transport),
		Phase:       phase,
		StackHash:   diagnosticStackHash(debug.Stack()),
		PanicClass:  panicClass(recovered),
		DetailError: ErrAdapterPanic.Error(),
	})
}

func bridgeDiagnosticTransport(transport string) string {
	if transport != "" {
		return transport
	}
	return "http_bridge"
}

func (s *BridgeSession) enqueueBridgeResultOrAbort(result BridgeEventResult, attemptID string, abortReason string, phases ...RecvDetailPhase) bool {
	if err := ValidateBridgeEventResult(result); err != nil {
		s.Abort("bridge_invalid_event_result")
		return false
	}
	phase := RecvDetailPhase("")
	if len(phases) > 0 {
		phase = phases[0]
	}
	return s.enqueueEventOrAbort(UpstreamEvent{
		Frame:        result.EmitFrame,
		Usage:        result.Usage,
		AttemptID:    attemptID,
		DetailOrigin: result.Origin,
		DetailPhase:  phase,
		Err:          result.Err,
	}, abortReason)
}

func (s *BridgeSession) releaseActiveAndEnqueueBridgeResultOrAbort(stream requester.StreamReaderInterface[string], result BridgeEventResult, attemptID string, abortReason string, phases ...RecvDetailPhase) bool {
	if err := ValidateBridgeEventResult(result); err != nil {
		s.Abort("bridge_invalid_event_result")
		return false
	}
	phase := RecvDetailPhase("")
	if len(phases) > 0 {
		phase = phases[0]
	}
	return s.releaseActiveAndEnqueueEventOrAbort(stream, UpstreamEvent{
		Frame:        result.EmitFrame,
		Usage:        result.Usage,
		AttemptID:    attemptID,
		DetailOrigin: result.Origin,
		DetailPhase:  phase,
		Err:          result.Err,
	}, abortReason)
}

func (s *BridgeSession) releaseActiveAndEnqueueBridgeResultBlocking(stream requester.StreamReaderInterface[string], result BridgeEventResult, attemptID string, phases ...RecvDetailPhase) bool {
	if err := ValidateBridgeEventResult(result); err != nil {
		s.Abort("bridge_invalid_event_result")
		return false
	}
	phase := RecvDetailPhase("")
	if len(phases) > 0 {
		phase = phases[0]
	}
	s.releaseActive(stream)
	return s.enqueueBlocking(UpstreamEvent{
		Frame:        result.EmitFrame,
		Usage:        result.Usage,
		AttemptID:    attemptID,
		DetailOrigin: result.Origin,
		DetailPhase:  phase,
		Err:          result.Err,
	})
}

func (s *BridgeSession) finishStreamAndEnqueueBridgeResultOrAbort(stream requester.StreamReaderInterface[string], streamID uint64, result BridgeEventResult, attemptID string, abortReason string, phases ...RecvDetailPhase) bool {
	if !s.markStreamFinished(streamID) {
		return true
	}
	if s.streamWasBackpressureCancelled(streamID) {
		s.clearBackpressureCancelled(streamID)
		return s.releaseActiveAndEnqueueBridgeResultBlocking(stream, result, attemptID, phases...)
	}
	return s.releaseActiveAndEnqueueBridgeResultOrAbort(stream, result, attemptID, abortReason, phases...)
}

func (s *BridgeSession) finishStreamAndEnqueueEventOrAbort(stream requester.StreamReaderInterface[string], streamID uint64, event UpstreamEvent, abortReason string) bool {
	if !s.markStreamFinished(streamID) {
		return true
	}
	return s.releaseActiveAndEnqueueEventOrAbort(stream, event, abortReason)
}

func (s *BridgeSession) releaseActiveAndEnqueueEventOrAbort(stream requester.StreamReaderInterface[string], event UpstreamEvent, abortReason string) bool {
	if s == nil {
		return false
	}
	var activeCancel context.CancelFunc
	var openingCancel context.CancelFunc
	var active requester.StreamReaderInterface[string]
	enqueued := false
	s.mu.Lock()
	if s.active == stream {
		activeCancel = s.activeCancel
		s.active = nil
		s.activeCancel = nil
		s.activeAttemptID = ""
		s.activeStreamID = 0
	}
	if !s.closed {
		select {
		case <-s.done:
		default:
			select {
			case s.recvCh <- event:
				enqueued = true
			default:
				s.closed = true
				active = s.active
				openingCancel = s.openingCancel
				s.active = nil
				s.activeCancel = nil
				s.activeAttemptID = ""
				s.activeStreamID = 0
				s.opening = false
				s.openingCancel = nil
				s.openingAttemptID = ""
				close(s.done)
			}
		}
	}
	s.mu.Unlock()
	if activeCancel != nil {
		activeCancel()
	}
	if openingCancel != nil {
		openingCancel()
	}
	if active != nil {
		active.Close()
	}
	return enqueued
}

func (s *BridgeSession) enqueueEventOrAbort(event UpstreamEvent, abortReason string) bool {
	if s.enqueue(event) {
		return true
	}
	s.Abort(abortReason)
	return false
}

func (s *BridgeSession) enqueue(event UpstreamEvent) bool {
	select {
	case <-s.done:
		return false
	default:
	}
	select {
	case s.recvCh <- event:
		return true
	default:
		return false
	}
}

func (s *BridgeSession) enqueueBlocking(event UpstreamEvent) bool {
	if s == nil {
		return false
	}
	select {
	case <-s.done:
		return false
	case s.recvCh <- event:
		return true
	}
}

func (s *BridgeSession) enqueueLocked(event UpstreamEvent) bool {
	if s == nil || s.closed {
		return false
	}
	select {
	case s.recvCh <- event:
		return true
	default:
		return false
	}
}

type bridgeSSEAssembler struct {
	limit     int64
	dataParts []string
	bytes     int64
}

func newBridgeSSEAssembler(limit int64) *bridgeSSEAssembler {
	if limit <= 0 {
		limit = defaultBridgeMaxStreamEventBytes
	}
	return &bridgeSSEAssembler{limit: limit}
}

func (a *bridgeSSEAssembler) Consume(raw string) ([]string, error) {
	if a == nil {
		return []string{raw}, nil
	}
	normalized := strings.ReplaceAll(raw, "\r\n", "\n")
	normalized = strings.ReplaceAll(normalized, "\r", "\n")
	segments := strings.SplitAfter(normalized, "\n")
	outputs := make([]string, 0, 1)
	for _, segment := range segments {
		if segment == "" {
			continue
		}
		line := strings.TrimSuffix(segment, "\n")
		if strings.TrimSpace(line) == "" {
			if event := a.flushEvent(); event != "" {
				outputs = append(outputs, event)
			}
			continue
		}
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, ":"):
			continue
		case strings.HasPrefix(trimmed, "data:"):
			if err := a.addBytes(segment); err != nil {
				return nil, err
			}
			a.dataParts = append(a.dataParts, strings.TrimSpace(strings.TrimPrefix(trimmed, "data:")))
		case strings.HasPrefix(trimmed, "event:"),
			strings.HasPrefix(trimmed, "id:"),
			strings.HasPrefix(trimmed, "retry:"):
			continue
		default:
			if len(a.dataParts) == 0 && len(segments) == 1 {
				return []string{raw}, nil
			}
			return nil, ErrInvalidBridgeStreamPayload
		}
	}
	if !strings.HasSuffix(normalized, "\n") {
		if event := a.flushEvent(); event != "" {
			outputs = append(outputs, event)
		}
	}
	return outputs, nil
}

func (a *bridgeSSEAssembler) Flush() ([]string, error) {
	if a == nil {
		return nil, nil
	}
	if event := a.flushEvent(); event != "" {
		return []string{event}, nil
	}
	return nil, nil
}

func (a *bridgeSSEAssembler) addBytes(value string) error {
	a.bytes += int64(len(value))
	if a.limit > 0 && a.bytes > a.limit {
		return requester.ErrStreamLineTooLarge
	}
	return nil
}

func (a *bridgeSSEAssembler) flushEvent() string {
	if len(a.dataParts) == 0 {
		a.bytes = 0
		return ""
	}
	var builder strings.Builder
	for _, part := range a.dataParts {
		builder.WriteString("data: ")
		builder.WriteString(part)
		builder.WriteString("\n")
	}
	a.dataParts = nil
	a.bytes = 0
	return builder.String()
}

func bridgeStreamPayload(raw string) ([]byte, bool, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return nil, false, nil
	}
	dataParts := make([]string, 0, 1)
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		switch {
		case strings.HasPrefix(line, "data:"):
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data != "" && data != "[DONE]" {
				dataParts = append(dataParts, data)
			}
		case strings.HasPrefix(line, "event:"), strings.HasPrefix(line, "id:"), strings.HasPrefix(line, "retry:"):
			continue
		default:
			return nil, false, ErrInvalidBridgeStreamPayload
		}
	}
	if len(dataParts) == 0 {
		return nil, false, nil
	}
	payload := []byte(strings.Join(dataParts, "\n"))
	if !json.Valid(payload) {
		return nil, false, ErrInvalidBridgeStreamPayload
	}
	if err := ValidateProviderEventPayload(payload); err != nil {
		return nil, false, ErrInvalidBridgeStreamPayload
	}
	if classified := ClassifyResponsesWSEvent(payload); classified.Malformed {
		return nil, false, ErrInvalidBridgeStreamPayload
	}
	return payload, true, nil
}

func bridgeStreamDone(raw string) bool {
	text := strings.TrimSpace(raw)
	if text == "" {
		return false
	}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		return strings.TrimSpace(strings.TrimPrefix(line, "data:")) == "[DONE]"
	}
	return text == "[DONE]"
}

var _ Upstream = (*BridgeSession)(nil)
var _ ControlSendCapable = (*BridgeSession)(nil)
var _ TransportSendCapable = (*BridgeSession)(nil)
