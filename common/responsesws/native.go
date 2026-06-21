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
	RecvQueueSize int
	Diagnostics   NativeDiagnosticHook
	ProviderName  string
	ChannelID     int
	Transport     string
}

// NativeSession adapts a provider websocket into the ResponsesWS upstream
// contract. It emits transport evidence; relay actor code owns accounting.
type NativeSession struct {
	conn         *wsconn.ManagedConn
	adapter      ProviderAdapter
	diag         NativeDiagnosticHook
	providerName string
	channelID    int
	transport    string

	recvCh     chan UpstreamEvent
	terminalCh chan UpstreamEvent
	done       chan struct{}

	sendMu          sync.Mutex
	attemptMu       sync.Mutex
	activeAttemptID string
	closeOnce       sync.Once
	doneOnce        sync.Once
	readPumpOnce    sync.Once
	writeMessage    func(wsconn.MessageType, []byte) error
}

func NewNativeSession(conn *wsconn.ManagedConn, adapter ProviderAdapter, options NativeSessionOptions) *NativeSession {
	queueSize := options.RecvQueueSize
	if queueSize <= 0 {
		queueSize = defaultNativeRecvQueueSize
	}
	return &NativeSession{
		conn:         conn,
		adapter:      adapter,
		diag:         options.Diagnostics,
		providerName: options.ProviderName,
		channelID:    options.ChannelID,
		transport:    options.Transport,
		recvCh:       make(chan UpstreamEvent, queueSize),
		terminalCh:   make(chan UpstreamEvent, 1),
		done:         make(chan struct{}),
	}
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
				_ = s.enqueue(UpstreamEvent{
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
	s.setActiveAttemptID(strings.TrimSpace(req.AttemptID))
	writeMessage := s.conn.WriteMessage
	if s.writeMessage != nil {
		writeMessage = s.writeMessage
	}
	err = writeMessage(mt, payload)
	if err != nil {
		return ResponsesWSTransportSendResult{Status: ResponsesWSTransportSendAmbiguous, Err: err}
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
	if event, ok := s.recvBufferedEvent(); ok {
		return event, nil
	}
	select {
	case terminal := <-s.terminalCh:
		if event, ok := s.recvBufferedEvent(); ok {
			return event, nil
		}
		return terminal, nil
	case event, ok := <-s.recvCh:
		if !ok {
			return UpstreamEvent{}, ErrUpstreamClosed
		}
		return event, nil
	case <-ctx.Done():
		return UpstreamEvent{}, ctx.Err()
	case <-s.done:
		if event, ok := s.recvBufferedEvent(); ok {
			return event, nil
		}
		select {
		case event := <-s.terminalCh:
			return event, nil
		default:
		}
		return UpstreamEvent{}, ErrUpstreamClosed
	}
}

func (s *NativeSession) recvBufferedEvent() (UpstreamEvent, bool) {
	if s == nil {
		return UpstreamEvent{}, false
	}
	select {
	case event, ok := <-s.recvCh:
		if !ok {
			return UpstreamEvent{}, false
		}
		return event, true
	default:
	}
	return UpstreamEvent{}, false
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
			s.handleProviderClose(info)
			s.closeDone()
		},
	}
	pump.Run(context.Background())
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
	_ = s.enqueue(event)
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

func (s *NativeSession) handleProviderClose(info wsconn.CloseInfo) {
	if s == nil {
		return
	}
	if s.adapter != nil && nativeCloseInfoCanMapProviderClose(info) {
		result := s.mapProviderClose(context.Background(), nativeProviderCloseInfoFromWSConn(info))
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
	select {
	case <-s.done:
		return false
	default:
	}
	select {
	case s.recvCh <- event:
		return true
	default:
		_ = s.enqueueTerminal(UpstreamEvent{
			AttemptID:    s.currentAttemptID(),
			DetailOrigin: RecvDetailOriginNativeBackpressure,
			Err:          ErrNativeQueueFull,
		})
		s.close(wsconn.CloseInfo{Kind: wsconn.CloseKindBackpressure, Code: wsconn.CloseTryAgainLater, Reason: "responses_ws_native_recv_backpressure", Err: ErrNativeQueueFull})
		return false
	}
}

func (s *NativeSession) enqueueTerminal(event UpstreamEvent) bool {
	if s == nil {
		return false
	}
	select {
	case <-s.done:
		return false
	default:
	}
	select {
	case s.terminalCh <- event:
		return true
	default:
		return true
	}
}

func (s *NativeSession) close(info wsconn.CloseInfo) {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() {
		if s.conn != nil {
			s.conn.Close(info)
		}
		s.closeDone()
	})
}

func (s *NativeSession) closeDone() {
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
