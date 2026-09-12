package responsesws

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime/debug"
	"strings"
	"sync"

	"one-api/common/wsconn"
)

const defaultNativeRecvQueueSize = 128

var ErrNativeQueueFull = errors.New("responses websocket native recv queue full")
var ErrNativeProtocol = errors.New("responses websocket native protocol error")
var ErrAdapterPanic = errors.New("responses websocket adapter panic")
var ErrNativeReadPumpPanic = errors.New("responses websocket native read pump panic")

// NativeSessionOptions configures the provider-native websocket transport.
type NativeSessionOptions struct {
	Credentials   []string
	Context       context.Context
	RecvQueueSize int
	Diagnostics   NativeDiagnosticHook
	// EventEnqueued 在事件被有序接收队列接纳后同步触发。它是可选观察点，
	// 不应阻塞正常调用方。
	EventEnqueued func(UpstreamEvent)
	ProviderName  string
	ChannelID     int
	Transport     string
}

// NativeSession adapts a provider websocket into the ResponsesWS upstream
// contract. It emits transport evidence; relay actor code owns accounting.
type NativeSession struct {
	credentials   []string
	conn          *wsconn.ManagedConn
	adapter       ProviderAdapter
	base          context.Context
	cancel        context.CancelFunc
	diag          NativeDiagnosticHook
	providerName  string
	channelID     int
	transport     string
	eventEnqueued func(UpstreamEvent)

	done   chan struct{}
	events nativeEventQueue

	sendMu             sync.Mutex
	attemptMu          sync.Mutex
	activeAttemptID    string
	closeOnce          sync.Once
	doneOnce           sync.Once
	readPumpOnce       sync.Once
	writeMessage       func(wsconn.MessageType, []byte) error
	writeMessageResult func(wsconn.MessageType, []byte) wsconn.WriteResult
}

// nativeQueuedEvent 由 nativeEventQueue.mu 保护。sequence 在事件被接纳时
// 分配，使并发生产者在 relay actor 观察事件前只有一个确定顺序。
type nativeQueuedEvent struct {
	event    UpstreamEvent
	sequence uint64
	terminal bool
}

// nativeEventQueue 是原生 read pump 使用的有序接收队列。普通事件使用配置
// 容量，另保留一个终态（或本地终态失败）槽位；终态不能绕过更早的普通帧，
// 同时保留原有背压边界。
type nativeEventQueue struct {
	mu            sync.Mutex
	items         []nativeQueuedEvent
	ordinaryLimit int
	ordinaryCount int
	terminalCount int
	nextSequence  uint64
	closed        bool
	notify        chan struct{}
}

func newNativeEventQueue(ordinaryLimit int) nativeEventQueue {
	return nativeEventQueue{
		// 由下方的 ordinaryCount/terminalCount 限制容量，避免从选项整数直接
		// 推导分配大小。
		items:         make([]nativeQueuedEvent, 0),
		ordinaryLimit: ordinaryLimit,
		notify:        make(chan struct{}, 1),
	}
}

// enqueue 仅在队列仍开放但无法接纳事件时返回 full=true。已关闭的队列拒绝
// 迟到生产者，不再触发新的背压关闭。
func (q *nativeEventQueue) enqueue(event UpstreamEvent, terminal bool) (accepted, full bool) {
	if q == nil {
		return false, false
	}
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return false, false
	}
	if terminal {
		if q.terminalCount >= 1 {
			q.mu.Unlock()
			return false, true
		}
	} else if q.ordinaryCount >= q.ordinaryLimit {
		q.mu.Unlock()
		return false, true
	}
	q.nextSequence++
	q.items = append(q.items, nativeQueuedEvent{
		event:    event,
		sequence: q.nextSequence,
		terminal: terminal,
	})
	if terminal {
		q.terminalCount++
	} else {
		q.ordinaryCount++
	}
	q.mu.Unlock()
	q.signal()
	return true, false
}

func (q *nativeEventQueue) dequeue() (UpstreamEvent, bool) {
	if q == nil {
		return UpstreamEvent{}, false
	}
	q.mu.Lock()
	if len(q.items) == 0 {
		q.mu.Unlock()
		return UpstreamEvent{}, false
	}
	queued := q.items[0]
	q.items[0] = nativeQueuedEvent{}
	q.items = q.items[1:]
	if queued.terminal {
		q.terminalCount--
	} else {
		q.ordinaryCount--
	}
	q.mu.Unlock()
	return queued.event, true
}

func (q *nativeEventQueue) signal() {
	if q == nil || q.notify == nil {
		return
	}
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

func (q *nativeEventQueue) close() {
	if q == nil {
		return
	}
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
}

func NewNativeSession(conn *wsconn.ManagedConn, adapter ProviderAdapter, options NativeSessionOptions) *NativeSession {
	queueSize := options.RecvQueueSize
	if queueSize <= 0 {
		queueSize = defaultNativeRecvQueueSize
	}
	base := options.Context
	if base == nil {
		base = context.Background()
	}
	base, cancel := context.WithCancel(context.WithoutCancel(base))
	return &NativeSession{
		credentials:   append([]string(nil), options.Credentials...),
		conn:          conn,
		adapter:       adapter,
		base:          base,
		cancel:        cancel,
		diag:          options.Diagnostics,
		eventEnqueued: options.EventEnqueued,
		providerName:  options.ProviderName,
		channelID:     options.ChannelID,
		transport:     options.Transport,
		done:          make(chan struct{}),
		events:        newNativeEventQueue(queueSize),
	}
}

func (s *NativeSession) ProviderCredentials() []string {
	return append([]string(nil), s.credentials...)
}

func (s *NativeSession) SendClientWithResult(ctx context.Context, req SendRequest) ResponsesWSTransportSendResult {
	return s.sendClient(ctx, req)
}

func (s *NativeSession) sendClient(ctx context.Context, req SendRequest) ResponsesWSTransportSendResult {
	if s == nil || s.conn == nil {
		return ResponsesWSTransportSendResult{Status: ResponsesWSTransportSendNotAttempted, Err: ErrUpstreamClosed}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return ResponsesWSTransportSendResult{Status: ResponsesWSTransportSendNotAttempted, Err: err}
	}
	frame := req.Frame
	if frame.IsZero() {
		return ResponsesWSTransportSendResult{Status: ResponsesWSTransportSendNotAttempted, Err: ErrInvalidFrame}
	}
	if err := validateClientAttemptID(req); err != nil {
		return ResponsesWSTransportSendResult{Status: ResponsesWSTransportSendNotAttempted, Err: err}
	}
	select {
	case <-s.done:
		return ResponsesWSTransportSendResult{Status: ResponsesWSTransportSendNotAttempted, Err: ErrUpstreamClosed}
	case <-s.conn.Done():
		return ResponsesWSTransportSendResult{Status: ResponsesWSTransportSendNotAttempted, Err: ErrUpstreamClosed}
	default:
	}

	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	if err := ctx.Err(); err != nil {
		return ResponsesWSTransportSendResult{Status: ResponsesWSTransportSendNotAttempted, Err: err}
	}
	select {
	case <-s.done:
		return ResponsesWSTransportSendResult{Status: ResponsesWSTransportSendNotAttempted, Err: ErrUpstreamClosed}
	case <-s.conn.Done():
		return ResponsesWSTransportSendResult{Status: ResponsesWSTransportSendNotAttempted, Err: ErrUpstreamClosed}
	default:
	}

	prepared := frame
	if s.adapter != nil {
		var err error
		prepared, err = s.prepareClientFrame(ctx, frame)
		if err != nil {
			if errors.Is(err, ErrAdapterPanic) {
				_ = s.enqueueTerminal(UpstreamEvent{
					AttemptID:    strings.TrimSpace(req.AttemptID),
					DetailOrigin: RecvDetailOriginAdapterPanic,
					DetailPhase:  RecvDetailPhasePrepareClientFrame,
					Err:          err,
				})
				s.close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Code: wsconn.CloseInternalServerErr, Reason: "adapter_panic_prepare_client_frame", Err: err})
			}
			return ResponsesWSTransportSendResult{Status: ResponsesWSTransportSendNotAttempted, Err: err}
		}
	}
	mt, payload, err := nativeMessageFromFrame(prepared)
	if err != nil {
		return ResponsesWSTransportSendResult{Status: ResponsesWSTransportSendNotAttempted, Err: err}
	}
	// 辅助命令只携带自己的完成关联，不能覆盖当前 create 的接收归属。
	if envelope, parseErr := ParseClientEventEnvelope(payload); parseErr == nil && envelope.Type == "response.create" {
		s.setActiveAttemptID(strings.TrimSpace(req.AttemptID))
	}
	var writeResult wsconn.WriteResult
	switch {
	case s.writeMessageResult != nil:
		writeResult = s.writeMessageResult(mt, payload)
	case s.writeMessage != nil:
		writeResult = wsconn.WriteResult{Attempted: true, Err: s.writeMessage(mt, payload)}
	default:
		writeResult = s.conn.WriteMessageResult(mt, payload)
	}
	if writeResult.Err != nil {
		status := ResponsesWSTransportSendAmbiguous
		if !writeResult.Attempted {
			status = ResponsesWSTransportSendNotAttempted
		}
		return ResponsesWSTransportSendResult{Status: status, Err: writeResult.Err}
	}
	return ResponsesWSTransportSendResult{Status: ResponsesWSTransportSendAttempted}
}

func (s *NativeSession) setActiveAttemptID(attemptID string) {
	if s == nil || attemptID == "" {
		return
	}
	s.attemptMu.Lock()
	s.activeAttemptID = attemptID
	s.attemptMu.Unlock()
}

func (s *NativeSession) currentAttemptID() string {
	if s == nil {
		return ""
	}
	s.attemptMu.Lock()
	defer s.attemptMu.Unlock()
	return s.activeAttemptID
}

func (s *NativeSession) Recv(ctx context.Context) (UpstreamEvent, error) {
	if s == nil {
		return UpstreamEvent{}, ErrUpstreamClosed
	}
	return s.recvEvent(ctx)
}

func (s *NativeSession) recvEvent(ctx context.Context) (UpstreamEvent, error) {
	if s == nil {
		return UpstreamEvent{}, ErrUpstreamClosed
	}
	s.startReadPump()
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		if event, ok := s.events.dequeue(); ok {
			return event, nil
		}
		select {
		case <-s.events.notify:
			// 循环顶部会再次检查队列。notify 仅负责唤醒，不参与事件排序。
		case <-ctx.Done():
			if event, ok := s.events.dequeue(); ok {
				return event, nil
			}
			return UpstreamEvent{}, ctx.Err()
		case <-s.done:
			if event, ok := s.events.dequeue(); ok {
				return event, nil
			}
			return UpstreamEvent{}, ErrUpstreamClosed
		}
	}
}

func (s *NativeSession) Abort(reason string) {
	if s == nil {
		return
	}
	_ = s.enqueue(UpstreamEvent{
		AttemptID:    s.currentAttemptID(),
		DetailOrigin: RecvDetailOriginNativeLocalAbort,
		Err:          ErrUpstreamClosed,
	})
	s.close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: reason})
}

func (s *NativeSession) Detach(reason string) {
	if s == nil {
		return
	}
	_ = s.enqueue(UpstreamEvent{
		AttemptID:    s.currentAttemptID(),
		DetailOrigin: RecvDetailOriginNativeLocalDetach,
		Err:          ErrUpstreamClosed,
	})
	s.close(wsconn.CloseInfo{Kind: wsconn.CloseKindGracefulShutdown, Reason: reason})
}

func (s *NativeSession) startReadPump() {
	if s == nil || s.conn == nil {
		return
	}
	s.readPumpOnce.Do(func() {
		go s.runReadPump()
	})
}

func (s *NativeSession) runReadPump() {
	ctx := s.base
	if ctx == nil {
		ctx = context.Background()
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			err := s.readPumpPanicError(recovered)
			_ = s.enqueue(UpstreamEvent{
				AttemptID:    s.currentAttemptID(),
				DetailOrigin: RecvDetailOriginNativeReadError,
				DetailPhase:  RecvDetailPhaseHandleProviderFrame,
				Err:          err,
			})
			s.close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Code: wsconn.CloseInternalServerErr, Reason: "native_read_pump_panic", Err: err})
			s.closeDone()
		}
	}()
	pump := wsconn.Pump{
		Conn: s.conn,
		Handle: func(ctx context.Context, mt wsconn.MessageType, payload []byte) {
			s.handleProviderMessage(ctx, mt, payload)
		},
		OnClose: func(info wsconn.CloseInfo) {
			s.handleProviderClose(ctx, info)
			if s.cancel != nil {
				s.cancel()
			}
			s.closeDone()
		},
	}
	pump.Run(ctx)
}

func (s *NativeSession) handleProviderMessage(ctx context.Context, mt wsconn.MessageType, payload []byte) {
	if mt == wsconn.BinaryMessage && !nativeAdapterSupportsBinary(s.adapter) {
		_ = s.enqueue(UpstreamEvent{
			AttemptID:    s.currentAttemptID(),
			DetailOrigin: RecvDetailOriginProviderMalformed,
			DetailPhase:  RecvDetailPhaseHandleProviderFrame,
			Err:          ErrNativeProtocol,
		})
		s.close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Code: wsconn.CloseUnsupportedData, Reason: "provider_binary_frame_unsupported", Err: ErrNativeProtocol})
		return
	}
	if s.adapter == nil {
		frame := NewTextFrame(append([]byte(nil), payload...))
		_ = s.enqueue(UpstreamEvent{
			Frame:        &frame,
			AttemptID:    s.currentAttemptID(),
			DetailOrigin: RecvDetailOriginProviderFrame,
			DetailPhase:  RecvDetailPhaseHandleProviderFrame,
		})
		return
	}
	result := s.handleProviderFrame(ctx, nativeFrameFromMessage(mt, payload))
	if err := ValidateProviderFrameResult(result); err != nil {
		if result.Err != nil {
			err = errors.Join(err, result.Err)
		}
		result = ProviderFrameResult{
			Origin:         RecvDetailOriginProviderMalformed,
			Err:            err,
			CloseTransport: true,
		}
	}
	if result.Filtered {
		return
	}
	event := UpstreamEvent{
		Usage:        result.Usage,
		AttemptID:    s.currentAttemptID(),
		DetailOrigin: result.Origin,
		DetailPhase:  RecvDetailPhaseHandleProviderFrame,
		Err:          result.Err,
	}
	if result.EmitFrame != nil {
		frame := *result.EmitFrame
		event.Frame = &frame
	}
	if result.Origin == RecvDetailOriginAdapterPanic {
		_ = s.enqueueTerminal(event)
	} else {
		_ = s.enqueue(event)
	}
	if result.CloseTransport {
		s.close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Code: wsconn.CloseProtocolError, Reason: string(result.Origin), Err: result.Err})
	}
}

func nativeAdapterSupportsBinary(adapter ProviderAdapter) bool {
	if adapter == nil {
		return false
	}
	capable, ok := adapter.(BinaryProviderFrameCapable)
	return ok && capable.SupportsBinaryProviderFrames()
}

func (s *NativeSession) handleProviderClose(ctx context.Context, info wsconn.CloseInfo) {
	if s == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if s.adapter != nil && nativeCloseInfoCanMapProviderClose(info) {
		result := s.mapProviderClose(ctx, nativeProviderCloseInfoFromWSConn(info))
		if result.ProviderClose != nil {
			_ = s.enqueue(UpstreamEvent{
				ProviderClose: &ProviderClose{
					Code:   result.ProviderClose.Code,
					Reason: result.ProviderClose.Reason,
					Err:    result.ProviderClose.Err,
				},
				AttemptID:    s.currentAttemptID(),
				DetailOrigin: result.Origin,
				DetailPhase:  RecvDetailPhaseMapProviderClose,
				Err:          result.Err,
			})
			return
		}
		if result.Err != nil {
			_ = s.enqueue(UpstreamEvent{
				AttemptID:    s.currentAttemptID(),
				DetailOrigin: result.Origin,
				DetailPhase:  RecvDetailPhaseMapProviderClose,
				Err:          result.Err,
			})
			return
		}
	}
	detail := RecvDetailOriginNativeReadError
	if info.Kind == wsconn.CloseKindPeerClose {
		detail = RecvDetailOriginNativeProviderClose
		_ = s.enqueue(UpstreamEvent{
			ProviderClose: &ProviderClose{Code: int(info.Code), Reason: info.Reason, Err: info.Err},
			AttemptID:     s.currentAttemptID(),
			DetailOrigin:  detail,
			DetailPhase:   RecvDetailPhaseMapProviderClose,
			Err:           info.Err,
		})
		return
	}
	if info.Kind == wsconn.CloseKindReadError && errors.Is(info.Err, io.EOF) {
		detail = RecvDetailOriginNativeProviderEOF
	}
	_ = s.enqueue(UpstreamEvent{
		AttemptID:    s.currentAttemptID(),
		DetailOrigin: detail,
		DetailPhase:  RecvDetailPhaseMapProviderClose,
		Err:          info.Err,
	})
}

func (s *NativeSession) prepareClientFrame(ctx context.Context, frame Frame) (prepared Frame, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = s.adapterPanicError(RecvDetailPhasePrepareClientFrame, recovered)
		}
	}()
	return s.adapter.PrepareClientFrame(ctx, frame)
}

func (s *NativeSession) handleProviderFrame(ctx context.Context, frame Frame) (result ProviderFrameResult) {
	defer func() {
		if recovered := recover(); recovered != nil {
			result = ProviderFrameResult{
				Origin:         RecvDetailOriginAdapterPanic,
				Err:            s.adapterPanicError(RecvDetailPhaseHandleProviderFrame, recovered),
				CloseTransport: true,
			}
		}
	}()
	return s.adapter.HandleProviderFrame(ctx, frame)
}

func (s *NativeSession) mapProviderClose(ctx context.Context, info ProviderCloseInfo) (result ProviderCloseResult) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err := s.adapterPanicError(RecvDetailPhaseMapProviderClose, recovered)
			result = ProviderCloseResult{
				Err:    err,
				Origin: RecvDetailOriginAdapterPanic,
			}
			if nativeProviderCloseInfo(info) {
				result.ProviderClose = &ProviderClose{
					Code:   info.Code,
					Reason: info.Reason,
					Err:    err,
				}
				result.Origin = RecvDetailOriginNativeProviderClose
			}
		}
	}()
	return s.adapter.MapProviderClose(ctx, info)
}

func nativeProviderCloseInfo(info ProviderCloseInfo) bool {
	return info.Kind == ProviderCloseKindPeerClose
}

func nativeCloseInfoCanMapProviderClose(info wsconn.CloseInfo) bool {
	// Only a peer close is provider evidence. Local normal/graceful/write/read
	// closes are proxy transport lifecycle and must not preserve pending quota.
	return info.Kind == wsconn.CloseKindPeerClose
}

func nativeFrameFromMessage(mt wsconn.MessageType, payload []byte) Frame {
	if mt == wsconn.BinaryMessage {
		return NewBinaryFrame(append([]byte(nil), payload...))
	}
	return NewTextFrame(append([]byte(nil), payload...))
}

func nativeProviderCloseInfoFromWSConn(info wsconn.CloseInfo) ProviderCloseInfo {
	return ProviderCloseInfo{
		Kind:   nativeProviderCloseKindFromWSConn(info.Kind),
		Code:   int(info.Code),
		Reason: info.Reason,
		Err:    info.Err,
	}
}

func nativeProviderCloseKindFromWSConn(kind wsconn.CloseKind) ProviderCloseKind {
	switch kind {
	case wsconn.CloseKindPeerClose:
		return ProviderCloseKindPeerClose
	case wsconn.CloseKindReadError:
		return ProviderCloseKindReadError
	case wsconn.CloseKindWriteError:
		return ProviderCloseKindWriteError
	case wsconn.CloseKindNormal:
		return ProviderCloseKindNormal
	case wsconn.CloseKindAbort:
		return ProviderCloseKindLocalAbort
	case wsconn.CloseKindGracefulShutdown:
		return ProviderCloseKindLocalClose
	default:
		return ProviderCloseKindUnknown
	}
}

func (s *NativeSession) adapterPanicError(phase RecvDetailPhase, recovered any) error {
	stackHash := diagnosticStackHash(debug.Stack())
	if s != nil && s.diag != nil {
		s.diag(NativeDiagnostic{
			Code:        "adapter_panic",
			Provider:    s.providerName,
			ChannelID:   s.channelID,
			Transport:   nativeDiagnosticTransport(s.transport),
			Phase:       phase,
			StackHash:   stackHash,
			PanicClass:  panicClass(recovered),
			DetailError: ErrAdapterPanic.Error(),
		})
	}
	return fmt.Errorf("%w: phase=%s stack=%s", ErrAdapterPanic, phase, stackHash)
}

func (s *NativeSession) readPumpPanicError(recovered any) error {
	stackHash := diagnosticStackHash(debug.Stack())
	if s != nil && s.diag != nil {
		s.diag(NativeDiagnostic{
			Code:        "native_read_pump_panic",
			Provider:    s.providerName,
			ChannelID:   s.channelID,
			Transport:   nativeDiagnosticTransport(s.transport),
			Phase:       RecvDetailPhaseHandleProviderFrame,
			StackHash:   stackHash,
			PanicClass:  panicClass(recovered),
			DetailError: ErrNativeReadPumpPanic.Error(),
		})
	}
	return fmt.Errorf("%w: stack=%s", ErrNativeReadPumpPanic, stackHash)
}

func nativeDiagnosticTransport(transport string) string {
	if transport != "" {
		return transport
	}
	return "native_ws"
}

func (s *NativeSession) enqueue(event UpstreamEvent) bool {
	if s == nil {
		return false
	}
	terminal := UpstreamEventHasProviderTerminalEvidence(event)
	if terminal {
		accepted, _ := s.events.enqueue(event, true)
		if !accepted {
			// Steering 可在消费上一响应终态前产生下一响应终态。保留终态
			// 专用槽位的同时，允许后续终态占用普通有界队列，不能静默丢弃。
			accepted, _ = s.events.enqueue(event, false)
		}
		if accepted {
			s.notifyEventEnqueued(event)
		} else {
			s.close(wsconn.CloseInfo{Kind: wsconn.CloseKindBackpressure, Code: wsconn.CloseTryAgainLater, Reason: "responses_ws_native_recv_backpressure", Err: ErrNativeQueueFull})
		}
		return accepted
	}
	if accepted, full := s.events.enqueue(event, false); accepted {
		s.notifyEventEnqueued(event)
		return true
	} else if !full {
		return false
	}
	backpressure := UpstreamEvent{
		AttemptID:    s.currentAttemptID(),
		DetailOrigin: RecvDetailOriginNativeBackpressure,
		Err:          ErrNativeQueueFull,
	}
	if accepted, _ := s.events.enqueue(backpressure, true); accepted {
		s.notifyEventEnqueued(backpressure)
	}
	s.close(wsconn.CloseInfo{Kind: wsconn.CloseKindBackpressure, Code: wsconn.CloseTryAgainLater, Reason: "responses_ws_native_recv_backpressure", Err: ErrNativeQueueFull})
	return false
}

func (s *NativeSession) enqueueTerminal(event UpstreamEvent) bool {
	if s == nil {
		return false
	}
	accepted, _ := s.events.enqueue(event, true)
	if accepted {
		s.notifyEventEnqueued(event)
	}
	return accepted
}

func (s *NativeSession) notifyEventEnqueued(event UpstreamEvent) {
	if s == nil || s.eventEnqueued == nil {
		return
	}
	s.eventEnqueued(event)
}

func (s *NativeSession) close(info wsconn.CloseInfo) {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() {
		if s.conn != nil {
			s.conn.Close(info)
		}
		if s.cancel != nil {
			s.cancel()
		}
		s.closeDone()
	})
}

func (s *NativeSession) closeDone() {
	if s == nil {
		return
	}
	s.events.close()
	s.doneOnce.Do(func() {
		close(s.done)
	})
}

func nativeMessageFromFrame(frame Frame) (wsconn.MessageType, []byte, error) {
	switch frame.Kind() {
	case FrameKindText:
		return wsconn.TextMessage, frame.Payload(), nil
	case FrameKindBinary:
		return wsconn.BinaryMessage, frame.Payload(), nil
	default:
		return 0, nil, ErrInvalidFrame
	}
}

var _ Upstream = (*NativeSession)(nil)
