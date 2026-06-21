package relay

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"one-api/common"
	"one-api/common/config"
	"one-api/common/logger"
	"one-api/common/requester"
	"one-api/common/responsesws"
	"one-api/common/wsconn"
	"one-api/middleware"
	"one-api/model"
	runtimesession "one-api/runtime/session"
	"one-api/types"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type ResponsesWSWriteMode int

const (
	ResponsesWSWriteProvider ResponsesWSWriteMode = iota
	ResponsesWSWriteProxyLocal
)

type responsesWSSendCommand struct {
	AttemptID                 string
	ResponseID                string
	DefaultPreviousResponseID string
	UpstreamSessionGeneration string
	SelectedChannelID         int
	Purpose                   ResponsesWSSendPurpose
	Session                   responsesws.Upstream
	Frame                     responsesws.Frame
	Context                   context.Context
}

type responsesWSClientWriter interface {
	WriteFrame(mt int, payload []byte, mode ResponsesWSWriteMode) error
	CloseWithCode(code int, reason string)
	Abort(reason string)
}

type responsesWSManagedClientWriter struct {
	conn *wsconn.ManagedConn
}

func newResponsesWSManagedClientWriter(conn *wsconn.ManagedConn) *responsesWSManagedClientWriter {
	if conn == nil {
		return nil
	}
	return &responsesWSManagedClientWriter{conn: conn}
}

func (w *responsesWSManagedClientWriter) WriteFrame(mt int, payload []byte, _ ResponsesWSWriteMode) error {
	if w == nil || w.conn == nil || len(payload) == 0 {
		return nil
	}
	if mt == responsesWSCloseMessageType {
		code, reason := parseResponsesWSClosePayload(payload)
		w.CloseWithCode(code, reason)
		return nil
	}
	return w.conn.WriteMessage(wsconn.MessageType(mt), payload)
}

func (w *responsesWSManagedClientWriter) CloseWithCode(code int, reason string) {
	if w == nil || w.conn == nil {
		return
	}
	w.conn.Close(wsconn.CloseInfo{
		Kind:   wsconn.CloseKindNormal,
		Code:   wsconn.SanitizeWireCloseCode(code),
		Reason: reason,
	})
}

func (w *responsesWSManagedClientWriter) Abort(reason string) {
	if w == nil || w.conn == nil {
		return
	}
	w.conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: strings.TrimSpace(reason)})
}

func recoverResponsesWSGoroutine(label string, onPanic func(reason string)) {
	if recovered := recover(); recovered != nil {
		reason := "responses_ws_" + strings.TrimSpace(label) + "_panic"
		stack := debug.Stack()
		logger.SysError(fmt.Sprintf("responses websocket %s panic: class=%s stack_hash=%s", label, responsesWSPanicClass(recovered), responsesWSStackHash(stack)))
		logger.SysDebug(fmt.Sprintf("stacktrace from responses websocket %s panic: %s", label, string(stack)))
		if onPanic != nil {
			onPanic(reason)
		}
	}
}

func responsesWSPanicClass(recovered any) string {
	if recovered == nil {
		return "nil"
	}
	return fmt.Sprintf("%T", recovered)
}

func responsesWSStackHash(stack []byte) string {
	sum := sha256.Sum256(stack)
	return hex.EncodeToString(sum[:8])
}

type ResponsesWSIOBridge struct {
	ctx    context.Context
	cancel context.CancelFunc
	writer responsesWSClientWriter
	actor  *ResponsesWSSessionActor
	armed  sync.Map
	wg     sync.WaitGroup
}

func NewResponsesWSManagedBridge(conn *wsconn.ManagedConn, actor *ResponsesWSSessionActor) *ResponsesWSIOBridge {
	ctx, cancel := context.WithCancel(context.Background())
	bridge := &ResponsesWSIOBridge{
		ctx:    ctx,
		cancel: cancel,
		writer: newResponsesWSManagedClientWriter(conn),
		actor:  actor,
	}
	return bridge
}

func newResponsesWSBridgeForTest(writer responsesWSClientWriter, actor *ResponsesWSSessionActor) *ResponsesWSIOBridge {
	ctx, cancel := context.WithCancel(context.Background())
	return &ResponsesWSIOBridge{
		ctx:    ctx,
		cancel: cancel,
		writer: writer,
		actor:  actor,
	}
}

func responsesWSClientWSConfig() wsconn.Config {
	inboundActivityTimeout := config.ResponsesWebsocketClientInboundActivityTimeout()
	writeTimeout := config.RealtimeWebsocketWriteTimeout()
	return wsconn.Config{
		Label:           "client-responses-ws",
		PingInterval:    config.ResponsesWebsocketClientPingInterval(),
		PongMissTimeout: config.ResponsesWebsocketClientPongMissTimeout(),
		InboundActivityTimeout: func() time.Duration {
			return inboundActivityTimeout
		},
		ReadLimit:    config.RealtimeWebsocketReadLimit(),
		WriteTimeout: func() time.Duration { return writeTimeout },
	}
}

func (b *ResponsesWSIOBridge) ArmProviderRecvPump(upstreamSessionGeneration string, selectedChannelID int, session responsesws.Upstream) {
	if b == nil || b.actor == nil || session == nil || upstreamSessionGeneration == "" {
		return
	}
	if _, loaded := b.armed.LoadOrStore(upstreamSessionGeneration, struct{}{}); loaded {
		return
	}
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		defer b.armed.Delete(upstreamSessionGeneration)
		defer recoverResponsesWSGoroutine("provider_recv_pump", func(reason string) {
			if b.actor != nil {
				if !b.actor.PostReliable(ResponsesWSEventTimeout{
					Reason:                    reason,
					UpstreamSessionGeneration: upstreamSessionGeneration,
					ChannelID:                 selectedChannelID,
				}) {
					return
				}
			}
		})
		for {
			event, err := session.Recv(b.ctx)
			receivedAt := time.Now()
			zeroChargeProofCandidate := responsesWSUpstreamZeroChargeProofCandidate(event)
			lifecycle := responsesWSProviderLifecyclePolicyForEvent(event)
			if responsesWSRecvHasProviderActivity(event, err) {
				b.actor.markActivity()
			}
			if event.ProviderClose != nil {
				if !b.actor.PostReliable(ResponsesWSEventProviderClosed{
					UpstreamSessionGeneration: upstreamSessionGeneration,
					ChannelID:                 selectedChannelID,
					AttemptID:                 event.AttemptID,
					Code:                      event.ProviderClose.Code,
					Reason:                    event.ProviderClose.Reason,
					Err:                       event.ProviderClose.Err,
					DetailOrigin:              event.DetailOrigin,
					DetailPhase:               event.DetailPhase,
					ReceivedAt:                receivedAt,
				}) {
					return
				}
				return
			}
			hasFrame := event.Frame != nil && event.Frame.PayloadLen() > 0
			if event.Usage != nil && !hasFrame {
				if !b.actor.PostReliable(ResponsesWSEventProviderUsageObserved{
					UpstreamSessionGeneration: upstreamSessionGeneration,
					ChannelID:                 selectedChannelID,
					AttemptID:                 event.AttemptID,
					ResponseID:                event.ResponseID,
					Usage:                     event.Usage,
					DetailOrigin:              event.DetailOrigin,
					DetailPhase:               event.DetailPhase,
					ReceivedAt:                receivedAt,
				}) {
					return
				}
			}
			if err != nil {
				if !b.postProviderFrameClientErrorAndFallback(responsesWSProviderRecvErrorDispatch{
					UpstreamSessionGeneration: upstreamSessionGeneration,
					ChannelID:                 selectedChannelID,
					Event:                     event,
					Err:                       err,
					ReceivedAt:                receivedAt,
					Recoverable: zeroChargeProofCandidate == responsesws.ZeroChargeProofCandidateProviderRejectedBeforeStream &&
						responsesws.BridgeOpenProviderErrorRecoverable(err),
					Fallback: responsesWSProviderRecvErrorFallbackRecvFailed,
				}) {
					return
				}
				return
			}
			if event.Err != nil {
				recoverableBridgeOpenProviderError := zeroChargeProofCandidate == responsesws.ZeroChargeProofCandidateProviderRejectedBeforeStream &&
					responsesws.BridgeOpenProviderErrorRecoverable(event.Err)
				if !b.postProviderFrameClientErrorAndFallback(responsesWSProviderRecvErrorDispatch{
					UpstreamSessionGeneration: upstreamSessionGeneration,
					ChannelID:                 selectedChannelID,
					Event:                     event,
					Err:                       event.Err,
					ReceivedAt:                receivedAt,
					Recoverable:               recoverableBridgeOpenProviderError,
					Fallback:                  responsesWSProviderRecvErrorFallbackLifecycleOrBusiness,
				}) {
					return
				}
				if recoverableBridgeOpenProviderError {
					continue
				}
				return
			}
			if !hasFrame {
				if lifecycle.DeliverRecvLifecycleEvent {
					if !b.actor.PostReliable(ResponsesWSEventProviderDownstream{
						UpstreamSessionGeneration: upstreamSessionGeneration,
						ChannelID:                 selectedChannelID,
						AttemptID:                 event.AttemptID,
						ResponseID:                event.ResponseID,
						Kind:                      ProviderDownstreamFrame,
						DetailOrigin:              event.DetailOrigin,
						DetailPhase:               event.DetailPhase,
						ReceivedAt:                receivedAt,
					}) {
						return
					}
				} else if lifecycle.DeliverRecvFailureLifecycleEvent {
					if !b.actor.PostReliable(ResponsesWSEventProviderRecvFailed{
						UpstreamSessionGeneration: upstreamSessionGeneration,
						ChannelID:                 selectedChannelID,
						AttemptID:                 event.AttemptID,
						Err:                       event.Err,
						DetailOrigin:              event.DetailOrigin,
						DetailPhase:               event.DetailPhase,
						ReceivedAt:                receivedAt,
					}) {
						return
					}
				}
				continue
			}
			if !b.actor.PostReliable(ResponsesWSEventProviderDownstream{
				UpstreamSessionGeneration: upstreamSessionGeneration,
				ChannelID:                 selectedChannelID,
				AttemptID:                 event.AttemptID,
				ResponseID:                event.ResponseID,
				Kind:                      ProviderDownstreamFrame,
				Frame:                     responsesWSCloneFramePtr(event.Frame),
				Usage:                     event.Usage,
				DetailOrigin:              event.DetailOrigin,
				DetailPhase:               event.DetailPhase,
				ReceivedAt:                receivedAt,
			}) {
				return
			}
		}
	}()
}

type responsesWSProviderRecvErrorFallback int

const (
	responsesWSProviderRecvErrorFallbackRecvFailed responsesWSProviderRecvErrorFallback = iota
	responsesWSProviderRecvErrorFallbackLifecycleOrBusiness
)

type responsesWSProviderRecvErrorDispatch struct {
	UpstreamSessionGeneration string
	ChannelID                 int
	Event                     responsesws.UpstreamEvent
	Err                       error
	ReceivedAt                time.Time
	Recoverable               bool
	Fallback                  responsesWSProviderRecvErrorFallback
}

func (b *ResponsesWSIOBridge) postProviderFrameClientErrorAndFallback(input responsesWSProviderRecvErrorDispatch) bool {
	if b == nil || b.actor == nil || input.Err == nil {
		return false
	}
	event := input.Event
	hasFrame := event.Frame != nil && event.Frame.PayloadLen() > 0
	deliveredPayload := hasFrame
	if deliveredPayload {
		if !b.actor.PostReliable(ResponsesWSEventProviderDownstream{
			UpstreamSessionGeneration: input.UpstreamSessionGeneration,
			ChannelID:                 input.ChannelID,
			AttemptID:                 event.AttemptID,
			ResponseID:                event.ResponseID,
			Kind:                      ProviderDownstreamFrame,
			Frame:                     responsesWSCloneFramePtr(event.Frame),
			Usage:                     event.Usage,
			DetailOrigin:              event.DetailOrigin,
			DetailPhase:               event.DetailPhase,
			ReceivedAt:                input.ReceivedAt,
		}) {
			return false
		}
	}
	deliveredClientErrorPayload := false
	if errorPayload := responsesws.ClientPayloadFromError(input.Err); len(errorPayload) > 0 {
		var deliveredFramePayload []byte
		if event.Frame != nil {
			deliveredFramePayload = event.Frame.Payload()
		}
		deliveredClientErrorPayload = deliveredPayload && bytes.Equal(errorPayload, deliveredFramePayload)
		if !deliveredClientErrorPayload {
			deliveredClientErrorPayload = true
			if !b.actor.PostReliable(responsesWSProxyLocalErrorEventFromUpstream(
				input.UpstreamSessionGeneration,
				input.ChannelID,
				event,
				errorPayload,
				responsesWSBridgeOpenProviderAPIError(input.Err),
				input.Recoverable,
			)) {
				return false
			}
		}
	}
	if deliveredPayload || deliveredClientErrorPayload {
		return true
	}
	switch input.Fallback {
	case responsesWSProviderRecvErrorFallbackLifecycleOrBusiness:
		if responsesWSProviderLifecyclePolicyForEvent(event).DeliverRecvFailureLifecycleEvent {
			return b.postProviderRecvFailed(input)
		}
		return b.actor.PostReliable(ResponsesWSEventProviderBusinessError{
			UpstreamSessionGeneration: input.UpstreamSessionGeneration,
			ChannelID:                 input.ChannelID,
			AttemptID:                 event.AttemptID,
			Err:                       input.Err,
			DetailOrigin:              event.DetailOrigin,
			DetailPhase:               event.DetailPhase,
		})
	default:
		return b.postProviderRecvFailed(input)
	}
}

func responsesWSProxyLocalErrorEventFromUpstream(upstreamSessionGeneration string, channelID int, event responsesws.UpstreamEvent, payload []byte, providerAPIError *types.OpenAIErrorWithStatusCode, recoverable bool) ResponsesWSEvent {
	switch {
	case event.DetailOrigin == responsesws.RecvDetailOriginBridgeOpenProviderError:
		return ResponsesWSEventBridgeOpenProviderError{
			UpstreamSessionGeneration: upstreamSessionGeneration,
			ChannelID:                 channelID,
			AttemptID:                 event.AttemptID,
			DetailPhase:               event.DetailPhase,
			Payload:                   append([]byte(nil), payload...),
			ProviderAPIError:          providerAPIError,
			Recoverable:               recoverable,
		}
	case event.DetailOrigin == responsesws.RecvDetailOriginBridgeStreamError && responsesws.IsBridgeOpenLocalError(event.Err):
		return ResponsesWSEventBridgeOpenLocalError{
			UpstreamSessionGeneration: upstreamSessionGeneration,
			ChannelID:                 channelID,
			AttemptID:                 event.AttemptID,
			DetailPhase:               event.DetailPhase,
			Payload:                   append([]byte(nil), payload...),
			Recoverable:               recoverable,
		}
	default:
		return ResponsesWSEventProxyLocalError{
			UpstreamSessionGeneration: upstreamSessionGeneration,
			ChannelID:                 channelID,
			AttemptID:                 event.AttemptID,
			DetailOrigin:              event.DetailOrigin,
			DetailPhase:               event.DetailPhase,
			Payload:                   append([]byte(nil), payload...),
			ProviderAPIError:          providerAPIError,
			Recoverable:               recoverable,
		}
	}
}

func (b *ResponsesWSIOBridge) postProviderRecvFailed(input responsesWSProviderRecvErrorDispatch) bool {
	if b == nil || b.actor == nil {
		return false
	}
	event := input.Event
	return b.actor.PostReliable(ResponsesWSEventProviderRecvFailed{
		UpstreamSessionGeneration: input.UpstreamSessionGeneration,
		ChannelID:                 input.ChannelID,
		AttemptID:                 event.AttemptID,
		Err:                       input.Err,
		DetailOrigin:              event.DetailOrigin,
		DetailPhase:               event.DetailPhase,
		ReceivedAt:                input.ReceivedAt,
	})
}

func responsesWSRecvHasProviderActivity(event responsesws.UpstreamEvent, err error) bool {
	// Liveness-only: accounting and provider terminal decisions must use the
	// actor-owned typed-origin evidence helpers after events enter the actor.
	if event.Usage != nil || event.Frame != nil && event.Frame.PayloadLen() > 0 || event.Err != nil || event.ProviderClose != nil {
		return true
	}
	return err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, responsesws.ErrUpstreamClosed)
}

func (a *ResponsesWSSessionActor) startSendWorker() {
	if a == nil {
		return
	}
	a.workers.sendOnce.Do(func() {
		go func() {
			defer recoverResponsesWSGoroutine("send_worker", func(reason string) {
				if a != nil {
					if !a.PostReliable(ResponsesWSEventTimeout{Reason: reason}) {
						return
					}
				}
			})
			for {
				select {
				case <-a.done:
					return
				case command := <-a.workers.sendCommands:
					a.handleSendCommand(command)
				}
			}
		}()
	})
}

func (a *ResponsesWSSessionActor) startControlWorker() {
	if a == nil {
		return
	}
	a.workers.controlOnce.Do(func() {
		go func() {
			defer recoverResponsesWSGoroutine("control_worker", func(reason string) {
				if a != nil {
					if !a.PostReliable(ResponsesWSEventTimeout{Reason: reason}) {
						return
					}
				}
			})
			for {
				select {
				case <-a.done:
					return
				case command := <-a.workers.controlCommands:
					a.handleControlCommand(command)
				}
			}
		}()
	})
}

func (a *ResponsesWSSessionActor) handleSendCommand(command responsesWSSendCommand) {
	defer recoverResponsesWSGoroutine("send_command", func(reason string) {
		if a != nil {
			if !a.PostReliable(ResponsesWSEventSendResult{
				AttemptID:                 command.AttemptID,
				ResponseID:                command.ResponseID,
				UpstreamSessionGeneration: command.UpstreamSessionGeneration,
				SelectedChannelID:         command.SelectedChannelID,
				Purpose:                   command.Purpose,
				TransportResult: responsesws.ResponsesWSTransportSendResult{
					Status: responsesws.ResponsesWSTransportSendAmbiguous,
					Err:    errors.New(reason),
				},
			}) {
				return
			}
		}
	})
	select {
	case <-a.done:
		if a != nil {
			if !a.PostReliable(ResponsesWSEventSendResult{
				AttemptID:                 command.AttemptID,
				ResponseID:                command.ResponseID,
				UpstreamSessionGeneration: command.UpstreamSessionGeneration,
				SelectedChannelID:         command.SelectedChannelID,
				Purpose:                   command.Purpose,
				TransportResult: responsesws.ResponsesWSTransportSendResult{
					Status: responsesws.ResponsesWSTransportSendNotAttempted,
					Err:    responsesws.ErrUpstreamClosed,
				},
			}) {
				return
			}
		}
		return
	default:
	}
	ctx := command.Context
	if ctx == nil {
		ctx = context.Background()
	}
	sendResult := responsesWSSendResultForCommand(ctx, command)
	if a != nil {
		if !a.postTransportSendResult(command, sendResult) {
			return
		}
	}
}

func (a *ResponsesWSSessionActor) handleControlCommand(command responsesWSSendCommand) {
	defer recoverResponsesWSGoroutine("control_command", func(reason string) {
		if a != nil {
			if !a.PostReliable(ResponsesWSEventSendResult{
				AttemptID:                 command.AttemptID,
				ResponseID:                command.ResponseID,
				UpstreamSessionGeneration: command.UpstreamSessionGeneration,
				SelectedChannelID:         command.SelectedChannelID,
				Purpose:                   command.Purpose,
				TransportResult: responsesws.ResponsesWSTransportSendResult{
					Status: responsesws.ResponsesWSTransportSendAmbiguous,
					Err:    errors.New(reason),
				},
			}) {
				return
			}
		}
	})
	select {
	case <-a.done:
		if a != nil {
			if !a.PostReliable(ResponsesWSEventSendResult{
				AttemptID:                 command.AttemptID,
				ResponseID:                command.ResponseID,
				UpstreamSessionGeneration: command.UpstreamSessionGeneration,
				SelectedChannelID:         command.SelectedChannelID,
				Purpose:                   command.Purpose,
				TransportResult: responsesws.ResponsesWSTransportSendResult{
					Status: responsesws.ResponsesWSTransportSendNotAttempted,
					Err:    responsesws.ErrUpstreamClosed,
				},
			}) {
				return
			}
		}
		return
	default:
	}
	controlCapable, ok := command.Session.(responsesws.ControlSendCapable)
	if !ok {
		if !a.PostReliable(ResponsesWSEventSendResult{
			AttemptID:                 command.AttemptID,
			ResponseID:                command.ResponseID,
			UpstreamSessionGeneration: command.UpstreamSessionGeneration,
			SelectedChannelID:         command.SelectedChannelID,
			Purpose:                   command.Purpose,
			TransportResult: responsesws.ResponsesWSTransportSendResult{
				Status: responsesws.ResponsesWSTransportSendNotAttempted,
				Err:    responsesws.ErrInvalidFrame,
			},
		}) {
			return
		}
		return
	}
	ctx := command.Context
	if ctx == nil {
		ctx = context.Background()
	}
	sendResult := controlCapable.SendControl(ctx, responsesws.SendRequest{
		AttemptID: command.AttemptID,
		Frame:     command.Frame,
	})
	if a != nil {
		if !a.postTransportSendResult(command, sendResult) {
			return
		}
	}
}

func (a *ResponsesWSSessionActor) postTransportSendResult(command responsesWSSendCommand, result responsesws.ResponsesWSTransportSendResult) bool {
	if a == nil {
		return false
	}
	if err := responsesws.ValidateResponsesWSTransportSendResult(result); err != nil {
		if result.Err != nil {
			err = errors.Join(result.Err, err)
		}
		return a.PostReliable(ResponsesWSEventTransportContractViolation{
			AttemptID:                 command.AttemptID,
			ResponseID:                command.ResponseID,
			UpstreamSessionGeneration: command.UpstreamSessionGeneration,
			SelectedChannelID:         command.SelectedChannelID,
			Purpose:                   command.Purpose,
			TransportResult:           result,
			Err:                       err,
		})
	}
	return a.PostReliable(ResponsesWSEventSendResult{
		AttemptID:                 command.AttemptID,
		ResponseID:                command.ResponseID,
		UpstreamSessionGeneration: command.UpstreamSessionGeneration,
		SelectedChannelID:         command.SelectedChannelID,
		Purpose:                   command.Purpose,
		TransportResult:           result,
	})
}

func responsesWSSendResultForCommand(ctx context.Context, command responsesWSSendCommand) responsesws.ResponsesWSTransportSendResult {
	if command.Session == nil {
		return responsesws.ResponsesWSTransportSendResult{Status: responsesws.ResponsesWSTransportSendNotAttempted, Err: responsesws.ErrUpstreamClosed}
	}
	return command.Session.SendClientWithResult(ctx, responsesws.SendRequest{
		AttemptID:                 command.AttemptID,
		Frame:                     command.Frame,
		DefaultPreviousResponseID: command.DefaultPreviousResponseID,
	})
}

func responsesWSTransportSendStatus(result responsesws.ResponsesWSTransportSendResult) responsesws.ResponsesWSTransportSendStatus {
	if result.Status == "" {
		return ""
	}
	if err := responsesws.ValidateResponsesWSTransportSendResult(result); err != nil {
		return ""
	}
	return result.Status
}

func responsesWSFrameFromWireMessage(messageType int, payload []byte) responsesws.Frame {
	if messageType == responsesWSBinaryMessageType {
		return responsesws.NewBinaryFrame(payload)
	}
	return responsesws.NewTextFrame(payload)
}

func responsesWSCloneFramePtr(frame *responsesws.Frame) *responsesws.Frame {
	if frame == nil {
		return nil
	}
	cloned := *frame
	return &cloned
}

func responsesWSProviderClosePayload(code int, reason string) []byte {
	sanitized := wsconn.SanitizeWireCloseCode(code)
	return wsconn.SafeCloseMessage(sanitized, responsesWSCloseReason(reason))
}

func (a *ResponsesWSSessionActor) SendProviderFrame(attemptID string, selectedChannelID int, session responsesws.Upstream, frame responsesws.Frame) bool {
	if a == nil || session == nil {
		return false
	}
	a.startSendWorker()
	previousResponseIDDefault := a.bridgeDefaultPreviousResponseID(session)
	a.recordPendingAttemptedPreviousResponseID(attemptID, previousResponseIDDefault)
	ctx := context.Background()
	if actorCtx := a.Context(); actorCtx != nil && actorCtx.Request != nil {
		ctx = actorCtx.Request.Context()
	}
	var cancel context.CancelFunc
	purpose := responsesWSSendPurposeFromAttempt(attemptID)
	if purpose == ResponsesWSSendPurposeResponseCreate && strings.TrimSpace(attemptID) != "" {
		ctx, cancel = context.WithCancel(ctx)
		a.setPendingSendCancel(attemptID, cancel)
	}
	command := responsesWSSendCommand{
		AttemptID:                 attemptID,
		DefaultPreviousResponseID: previousResponseIDDefault,
		UpstreamSessionGeneration: a.upstream.sessionGeneration,
		SelectedChannelID:         selectedChannelID,
		Purpose:                   purpose,
		Session:                   session,
		Frame:                     frame,
		Context:                   ctx,
	}
	select {
	case <-a.done:
		if cancel != nil {
			cancel()
			a.clearPendingSendCancel(attemptID)
		}
		return false
	case a.workers.sendCommands <- command:
		return true
	default:
		if cancel != nil {
			cancel()
			a.clearPendingSendCancel(attemptID)
		}
		return false
	}
}

func (a *ResponsesWSSessionActor) SendProviderControlFrame(targetAttemptID string, selectedChannelID int, session responsesws.Upstream, frame responsesws.Frame) bool {
	if a == nil || session == nil {
		return false
	}
	if _, ok := session.(responsesws.ControlSendCapable); !ok {
		a.startSendWorker()
		ctx := context.Background()
		if actorCtx := a.Context(); actorCtx != nil && actorCtx.Request != nil {
			ctx = actorCtx.Request.Context()
		}
		command := responsesWSSendCommand{
			AttemptID:                 targetAttemptID,
			UpstreamSessionGeneration: a.upstream.sessionGeneration,
			SelectedChannelID:         selectedChannelID,
			Purpose:                   ResponsesWSSendPurposeResponseCancel,
			Session:                   session,
			Frame:                     frame,
			Context:                   ctx,
		}
		select {
		case <-a.done:
			return false
		case a.workers.sendCommands <- command:
			return true
		default:
			return false
		}
	}
	a.startControlWorker()
	ctx := context.Background()
	if actorCtx := a.Context(); actorCtx != nil && actorCtx.Request != nil {
		ctx = actorCtx.Request.Context()
	}
	command := responsesWSSendCommand{
		AttemptID:                 targetAttemptID,
		UpstreamSessionGeneration: a.upstream.sessionGeneration,
		SelectedChannelID:         selectedChannelID,
		Purpose:                   ResponsesWSSendPurposeResponseCancel,
		Session:                   session,
		Frame:                     frame,
		Context:                   ctx,
	}
	select {
	case <-a.done:
		return false
	case a.workers.controlCommands <- command:
		return true
	default:
		return false
	}
}

func (a *ResponsesWSSessionActor) recordPendingAttemptedPreviousResponseID(attemptID string, previousResponseIDDefault string) {
	if a == nil || strings.TrimSpace(attemptID) == "" || a.turns.pending.attempt == nil || a.turns.pending.attempt.AttemptID != attemptID {
		return
	}
	if strings.TrimSpace(a.turns.pending.attempt.AttemptedPreviousResponseID) != "" {
		return
	}
	a.turns.pending.attempt.AttemptedPreviousResponseID = strings.TrimSpace(previousResponseIDDefault)
}

func (a *ResponsesWSSessionActor) bridgeDefaultPreviousResponseID(session responsesws.Upstream) string {
	if a == nil || a.turns.history.lastFinal == nil {
		return ""
	}
	capable, ok := session.(responsesws.BridgeContinuationDefaultCapable)
	if !ok || !capable.SupportsBridgeContinuationDefault() {
		return ""
	}
	return strings.TrimSpace(a.turns.history.lastFinal.ID)
}

func responsesWSSendPurposeFromAttempt(attemptID string) ResponsesWSSendPurpose {
	if strings.TrimSpace(attemptID) != "" {
		return ResponsesWSSendPurposeResponseCreate
	}
	return ResponsesWSSendPurposeResponseCancel
}

func (b *ResponsesWSIOBridge) WriteClientFrame(mt int, payload []byte, mode ResponsesWSWriteMode) error {
	if b == nil || len(payload) == 0 {
		return nil
	}
	if b.writer == nil {
		return nil
	}
	return b.writer.WriteFrame(mt, payload, mode)
}

func (b *ResponsesWSIOBridge) WriteClientTypedFrame(frame responsesws.Frame, mode ResponsesWSWriteMode) error {
	if frame.IsZero() {
		return nil
	}
	messageType := responsesWSTextMessageType
	if frame.Kind() == responsesws.FrameKindBinary {
		messageType = responsesWSBinaryMessageType
	}
	return b.WriteClientFrame(messageType, frame.Payload(), mode)
}

func (b *ResponsesWSIOBridge) WriteCloseControl(code int, reason string) {
	if b == nil {
		return
	}
	if b.writer == nil {
		return
	}
	b.writer.CloseWithCode(code, reason)
}

func (b *ResponsesWSIOBridge) AbortSession(session responsesws.Upstream, reason string) {
	if session != nil {
		session.Abort(strings.TrimSpace(reason))
	}
}

func (b *ResponsesWSIOBridge) Close() {
	if b == nil {
		return
	}
	b.cancel()
	if b.writer != nil {
		b.writer.Abort("bridge_closed")
	}
	b.wg.Wait()
}

func parseResponsesWSClosePayload(payload []byte) (int, string) {
	if len(payload) < 2 {
		return int(wsconn.CloseNormalClosure), ""
	}
	code := int(binary.BigEndian.Uint16(payload[:2]))
	return code, string(payload[2:])
}

type responsesWSFrameDiagnostics struct {
	EventType           string
	Generate            string
	PreviousResponse    string
	Model               string
	SubagentPresent     bool
	SubagentBytes       int
	SubagentHash        string
	ParentThreadPresent bool
	ParentThreadBytes   int
	ParentThreadHash    string
	TurnRequestKind     string
	TurnMetadataBytes   int
	PayloadBytes        int
}

const responsesWSFrameDiagnosticValueLimit = 256

func responsesWSFrameDiagnosticsFromRaw(raw []byte) responsesWSFrameDiagnostics {
	diag := responsesWSFrameDiagnostics{PayloadBytes: len(raw)}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return diag
	}
	diag.EventType = responsesWSRedactAndLimitDiagnostic(jsonStringField(object, "type"))
	diag.Model = responsesWSRedactAndLimitDiagnostic(jsonStringField(object, "model"))
	diag.PreviousResponse = responsesWSRedactAndLimitDiagnostic(jsonStringField(object, "previous_response_id"))
	diag.Generate = jsonBoolPresence(object, "generate")

	var metadata map[string]json.RawMessage
	if rawMetadata, ok := object["client_metadata"]; ok {
		if err := json.Unmarshal(rawMetadata, &metadata); err == nil {
			subagent := jsonStringField(metadata, "x-openai-subagent")
			diag.SubagentPresent = strings.TrimSpace(subagent) != ""
			diag.SubagentBytes = len(subagent)
			diag.SubagentHash = responsesWSDiagnosticHash(subagent)
			parentThreadID := jsonStringField(metadata, "x-codex-parent-thread-id")
			diag.ParentThreadPresent = strings.TrimSpace(parentThreadID) != ""
			diag.ParentThreadBytes = len(parentThreadID)
			diag.ParentThreadHash = responsesWSDiagnosticHash(parentThreadID)
			turnMetadata := jsonStringField(metadata, "x-codex-turn-metadata")
			diag.TurnMetadataBytes = len(turnMetadata)
			diag.TurnRequestKind = responsesWSRedactAndLimitDiagnostic(responsesWSTurnRequestKind(turnMetadata))
		}
	}
	return diag
}

func responsesWSSafeDiagnosticValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	value = strings.NewReplacer("\r", "\\r", "\n", "\\n", "\t", "\\t").Replace(value)
	runes := []rune(value)
	if len(runes) <= responsesWSFrameDiagnosticValueLimit {
		return value
	}
	return string(runes[:responsesWSFrameDiagnosticValueLimit]) + "...(truncated)"
}

func responsesWSDiagnosticHash(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:16]
}

func jsonStringField(object map[string]json.RawMessage, key string) string {
	if object == nil {
		return ""
	}
	raw, ok := object[key]
	if !ok {
		return ""
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	return strings.TrimSpace(value)
}

func jsonBoolPresence(object map[string]json.RawMessage, key string) string {
	if object == nil {
		return "absent"
	}
	raw, ok := object[key]
	if !ok {
		return "absent"
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return "non_bool"
	}
	if value {
		return "true"
	}
	return "false"
}

func responsesWSTurnRequestKind(turnMetadata string) string {
	turnMetadata = strings.TrimSpace(turnMetadata)
	if turnMetadata == "" {
		return ""
	}
	var metadata map[string]json.RawMessage
	if err := json.Unmarshal([]byte(turnMetadata), &metadata); err != nil {
		return ""
	}
	return jsonStringField(metadata, "request_kind")
}

func logResponsesWSFirstFrame(ctx context.Context, diag responsesWSFrameDiagnostics) {
	logger.LogDebug(ctx, fmt.Sprintf(
		"responses websocket first frame: type=%s model=%s generate=%s has_previous_response_id=%t subagent_present=%t subagent_bytes=%d subagent_hash=%s parent_thread_present=%t parent_thread_bytes=%d parent_thread_hash=%s request_kind=%s turn_metadata_bytes=%d payload_bytes=%d",
		diag.EventType,
		diag.Model,
		diag.Generate,
		diag.PreviousResponse != "",
		diag.SubagentPresent,
		diag.SubagentBytes,
		diag.SubagentHash,
		diag.ParentThreadPresent,
		diag.ParentThreadBytes,
		diag.ParentThreadHash,
		diag.TurnRequestKind,
		diag.TurnMetadataBytes,
		diag.PayloadBytes,
	))
}

// ensureResponsesWSConnectionSessionID assigns a per-downstream-connection
// session id for provider execution-session isolation. Client x-session-id
// remains available to routing/prompt-cache code through the request snapshot,
// but ResponsesWS live provider WebSockets must not be shared across separate
// downstream WebSocket connections.
func ensureResponsesWSConnectionSessionID(c *gin.Context) string {
	if c == nil {
		return ""
	}
	if existing := strings.TrimSpace(c.GetString(responsesWSConnectionSessionIDKey)); existing != "" {
		return existing
	}
	sessionID := "responses-ws:" + uuid.NewString()
	c.Set(responsesWSConnectionSessionIDKey, sessionID)
	return sessionID
}

type responsesWSSessionState int

const (
	// Keep this compact enum aligned with docs/dev/responses-ws-architecture.md
	// "Actor 状态机速查"; pendingTurnPhase is only a correlation aid.
	responsesWSStateOpening responsesWSSessionState = iota
	responsesWSStatePendingPrepare
	responsesWSStatePendingSend
	responsesWSStateInFlight
	responsesWSStateIdle
	responsesWSStateClosed
)

type responsesWSPendingTurnPhase int

const (
	responsesWSPendingTurnNone responsesWSPendingTurnPhase = iota
	responsesWSPendingTurnOpening
	responsesWSPendingTurnPrepare
	responsesWSPendingTurnSend
)

const (
	responsesWSBackpressurePostTimeout      = 5 * time.Second
	responsesWSIdleWatchdogMaxInterval      = 5 * time.Second
	responsesWSHandlerPanicCleanupGraceTime = 5 * time.Second
)

type ResponsesWSSessionActor struct {
	events   chan ResponsesWSEvent
	done     chan struct{}
	doneOnce sync.Once

	io       responsesWSIOState
	snapshot responsesWSSnapshotState
	lease    responsesWSLeaseState
	upstream responsesWSUpstreamState

	turns    responsesWSTurnSlots
	workers  responsesWSWorkerState
	closing  responsesWSCloseState
	watchdog responsesWSWatchdogState

	state               responsesWSSessionState
	reliablePostTimeout time.Duration
}

func NewResponsesWSSessionActor(c *gin.Context) *ResponsesWSSessionActor {
	actor := &ResponsesWSSessionActor{
		events: make(chan ResponsesWSEvent, responsesWSEventQueueSize),
		done:   make(chan struct{}),
		workers: responsesWSWorkerState{
			sendCommands:    make(chan responsesWSSendCommand, responsesWSSendQueueSize),
			controlCommands: make(chan responsesWSSendCommand, responsesWSSendQueueSize),
		},
		state:               responsesWSStateOpening,
		reliablePostTimeout: defaultResponsesWSReliablePostTimeout,
	}
	actor.RefreshContext(c)
	actor.markActivity()
	return actor
}

func (a *ResponsesWSSessionActor) RefreshContext(c *gin.Context) {
	if a == nil {
		return
	}
	var snapshot *ResponsesWSRequestSnapshot
	if c != nil {
		snapshot = NewResponsesWSRequestSnapshot(c)
	}
	a.setSnapshot(snapshot)
}

func (a *ResponsesWSSessionActor) Context() *gin.Context {
	snapshot := a.snapshotClone()
	if snapshot == nil {
		return nil
	}
	return snapshot.Context()
}

func (a *ResponsesWSSessionActor) setSnapshot(snapshot *ResponsesWSRequestSnapshot) {
	if a == nil {
		return
	}
	a.snapshot.mu.Lock()
	a.snapshot.snapshot = snapshot
	a.snapshot.mu.Unlock()
}

func (a *ResponsesWSSessionActor) snapshotClone() *ResponsesWSRequestSnapshot {
	if a == nil {
		return nil
	}
	a.snapshot.mu.RLock()
	defer a.snapshot.mu.RUnlock()
	return a.snapshot.snapshot.Clone()
}

func (a *ResponsesWSSessionActor) mutateSnapshot(mutator func(*ResponsesWSRequestSnapshot)) {
	if a == nil || mutator == nil {
		return
	}
	a.snapshot.mu.Lock()
	defer a.snapshot.mu.Unlock()
	if a.snapshot.snapshot != nil {
		mutator(a.snapshot.snapshot)
	}
}

func (a *ResponsesWSSessionActor) SetBridge(bridge *ResponsesWSIOBridge) {
	a.io.bridge = bridge
}

func (a *ResponsesWSSessionActor) SetClientConn(conn *wsconn.ManagedConn) {
	if a == nil {
		return
	}
	a.io.client = conn
}

func (a *ResponsesWSSessionActor) Start() {
	a.workers.runWG.Add(2)
	go func() {
		defer a.workers.runWG.Done()
		a.loop()
	}()
	go func() {
		defer a.workers.runWG.Done()
		a.idleWatchdog()
	}()
}

func (a *ResponsesWSSessionActor) Done() <-chan struct{} {
	return a.done
}

func (a *ResponsesWSSessionActor) waitStartedGoroutines() {
	if a == nil {
		return
	}
	a.workers.runWG.Wait()
}

func (a *ResponsesWSSessionActor) reliablePostTimeoutValue() time.Duration {
	if a == nil || a.reliablePostTimeout <= 0 {
		return defaultResponsesWSReliablePostTimeout
	}
	return a.reliablePostTimeout
}

func (a *ResponsesWSSessionActor) Post(event ResponsesWSEvent) bool {
	if a == nil || a.closing.closed.Load() {
		return false
	}
	select {
	case a.events <- event:
		return true
	default:
		if a.closing.backpressurePosted.CompareAndSwap(false, true) {
			go func() {
				defer recoverResponsesWSGoroutine("backpressure_post", nil)
				timer := time.NewTimer(responsesWSBackpressurePostTimeout)
				defer timer.Stop()
				select {
				case a.events <- ResponsesWSEventTimeout{Reason: "responses_ws_event_backpressure"}:
				case <-a.done:
				case <-timer.C:
					a.logErrorf("responses websocket backpressure timeout post timed out")
				}
			}()
		}
		return false
	}
}

func (a *ResponsesWSSessionActor) PostReliable(event ResponsesWSEvent) bool {
	if a == nil || a.closing.closed.Load() {
		return false
	}
	timeout := a.reliablePostTimeoutValue()
	ok, timedOut := a.postEventBounded(event, timeout)
	if ok {
		return true
	}
	if timedOut {
		a.handleReliablePostTimeout(event, timeout)
	}
	return false
}

func (a *ResponsesWSSessionActor) postEventBounded(event ResponsesWSEvent, timeout time.Duration) (bool, bool) {
	if a == nil || a.closing.closed.Load() {
		return false, false
	}
	select {
	case a.events <- event:
		return true, false
	case <-a.done:
		return false, false
	default:
	}
	if timeout <= 0 {
		select {
		case a.events <- event:
			return true, false
		case <-a.done:
			return false, false
		default:
			return false, true
		}
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case a.events <- event:
		return true, false
	case <-a.done:
		return false, false
	case <-timer.C:
		return false, true
	}
}

func (a *ResponsesWSSessionActor) handleReliablePostTimeout(event ResponsesWSEvent, timeout time.Duration) {
	if a == nil {
		return
	}
	eventType := responsesWSEventTypeLabel(event)
	a.logErrorf("responses websocket reliable event post timed out: event_type=%s timeout=%s", eventType, timeout)
	recordResponsesWSEventPostTimeout(eventType)
	if a.io.client != nil {
		a.io.client.Close(wsconn.CloseInfo{
			Kind:   wsconn.CloseKindBackpressure,
			Code:   wsconn.CloseTryAgainLater,
			Reason: "responses_ws_event_backpressure",
			Err:    errResponsesWSEventPostTimeout,
		})
	}
	a.requestCloseIntent("reliable_post_timeout")
}

func (a *ResponsesWSSessionActor) ReserveFirstTurnOpening(frame *responsesws.RawResponsesCreateFrame) string {
	opening := responsesWSOpeningTurn{
		openingID:  uuid.NewString(),
		firstFrame: frame,
		admission:  NewResponsesWSTurnAdmission(),
	}
	if err := a.turns.BeginOpening(opening); err != nil {
		a.logErrorf("responses websocket opening transition failed: %v", err)
		return ""
	}
	a.state = responsesWSStateOpening
	return a.turns.opening.openingID
}

func (a *ResponsesWSSessionActor) AttachUpstreamSession(session responsesws.Upstream, selectedChannelID int) string {
	a.upstream.session = session
	a.upstream.channelID = selectedChannelID
	a.upstream.sessionGeneration = uuid.NewString()
	return a.upstream.sessionGeneration
}

func (a *ResponsesWSSessionActor) BeginCandidate(attempt *ResponsesWSTurnAttempt) error {
	if a == nil || attempt == nil {
		return errors.New("attempt is required")
	}
	if a.closing.closed.Load() {
		return errors.New("responses websocket session is closed")
	}
	if err := attempt.BeginCandidate(a); err != nil {
		return err
	}
	a.turns.pending.phase = responsesWSPendingTurnPrepare
	a.state = responsesWSStatePendingPrepare
	return nil
}

func (a *ResponsesWSSessionActor) MarkPendingSend() {
	a.turns.pending.phase = responsesWSPendingTurnSend
	a.state = responsesWSStatePendingSend
}

func (a *ResponsesWSSessionActor) settlePendingAttemptBeforeLocalWrite(reason string, proof ResponsesWSZeroChargeProof) error {
	if a == nil {
		return nil
	}
	attempt := a.turns.pending.attempt
	if attempt != nil {
		a.clearPendingSendCancel(attempt.AttemptID)
		a.clearPendingCreateCancel(attempt.AttemptID)
		input := a.buildPendingSettlementInput(reason, proof)
		_, _, err := a.applyPendingSettlement(input)
		if err != nil {
			return err
		}
	}
	a.clearPendingTurn(reason)
	if !a.closing.closed.Load() {
		a.state = responsesWSStateIdle
	}
	return nil
}

func (a *ResponsesWSSessionActor) settlePendingAttemptOrClose(reason string, proof ResponsesWSZeroChargeProof) bool {
	if err := a.settlePendingAttemptBeforeLocalWrite(reason, proof); err != nil {
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusInternalServerError, "quota_rollback_failed", responsesWSStaticErrorMessage("quota_rollback_failed")))
		a.close("quota_rollback_failed")
		return false
	}
	return true
}

func (a *ResponsesWSSessionActor) markClientClosed(err error) {
	if a == nil {
		return
	}
	if err != nil && !isResponsesWSExpectedClientDisconnectError(err) {
		a.logDebugf("responses websocket client closed: %T: %v", err, err)
	}
	a.closing.clientClosed.Store(true)
	a.cancelSetup()
	if a.io.bridge != nil && a.io.bridge.cancel != nil {
		a.io.bridge.cancel()
	}
}

func (a *ResponsesWSSessionActor) isClientGone() bool {
	return a == nil || a.closing.closed.Load() || a.closing.clientClosed.Load()
}

func isResponsesWSExpectedClientDisconnectError(err error) bool {
	if err == nil {
		return false
	}
	// Codex and browsers commonly exit without completing the websocket close
	// handshake. Suppressing these transport-level disconnects keeps normal
	// shutdowns out of info logs; cleanup and quota finalization still run.
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, context.Canceled) {
		return true
	}
	if errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED) {
		return true
	}
	var managedCloseErr *wsconn.CloseError
	if errors.As(err, &managedCloseErr) {
		switch managedCloseErr.Code {
		case wsconn.CloseNormalClosure, wsconn.CloseGoingAway, wsconn.CloseNoStatusReceived, wsconn.CloseAbnormalClosure:
			return true
		}
	}
	message := strings.ToLower(strings.TrimSpace(err.Error()))
	if strings.Contains(message, "broken pipe") || strings.Contains(message, "connection reset by peer") || strings.Contains(message, "software caused connection abort") {
		return true
	}
	return false
}

func (a *ResponsesWSSessionActor) setSetupCancel(cancel context.CancelFunc) {
	if a == nil {
		return
	}
	a.workers.setupCancelMu.Lock()
	a.workers.setupCancel = cancel
	a.workers.setupCancelMu.Unlock()
}

func (a *ResponsesWSSessionActor) clearSetupCancel() {
	if a == nil {
		return
	}
	a.workers.setupCancelMu.Lock()
	a.workers.setupCancel = nil
	a.workers.setupCancelMu.Unlock()
}

func (a *ResponsesWSSessionActor) cancelSetup() {
	if a == nil {
		return
	}
	a.workers.setupCancelMu.Lock()
	cancel := a.workers.setupCancel
	a.workers.setupCancelMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (a *ResponsesWSSessionActor) setPendingSendCancel(attemptID string, cancel context.CancelFunc) {
	if a == nil || strings.TrimSpace(attemptID) == "" || cancel == nil {
		return
	}
	a.turns.pending.cancel.sendAttemptID = strings.TrimSpace(attemptID)
	a.turns.pending.cancel.sendCancel = cancel
}

func (a *ResponsesWSSessionActor) clearPendingSendCancel(attemptID string) {
	if a == nil {
		return
	}
	if strings.TrimSpace(attemptID) != "" && a.turns.pending.cancel.sendAttemptID != strings.TrimSpace(attemptID) {
		return
	}
	a.turns.pending.cancel.sendAttemptID = ""
	a.turns.pending.cancel.sendCancel = nil
}

func (a *ResponsesWSSessionActor) cancelPendingSend(attemptID string) {
	if a == nil || strings.TrimSpace(attemptID) == "" || a.turns.pending.cancel.sendAttemptID != strings.TrimSpace(attemptID) {
		return
	}
	if a.turns.pending.cancel.sendCancel != nil {
		a.turns.pending.cancel.sendCancel()
	}
}

func (a *ResponsesWSSessionActor) markPendingCreateCancel(attemptID string, event ResponsesWSEventClientFrame) {
	if a == nil || strings.TrimSpace(attemptID) == "" {
		return
	}
	a.turns.pending.cancel.createAttemptID = strings.TrimSpace(attemptID)
	a.turns.pending.cancel.createFrame = event.Frame
}

func (a *ResponsesWSSessionActor) hasPendingCreateCancel(attemptID string) bool {
	return a != nil && strings.TrimSpace(attemptID) != "" && a.turns.pending.cancel.createAttemptID == strings.TrimSpace(attemptID)
}

func (a *ResponsesWSSessionActor) clearPendingCreateCancel(attemptID string) {
	if a == nil {
		return
	}
	if strings.TrimSpace(attemptID) != "" && a.turns.pending.cancel.createAttemptID != strings.TrimSpace(attemptID) {
		return
	}
	a.turns.pending.cancel.createAttemptID = ""
	a.turns.pending.cancel.createFrame = responsesws.Frame{}
}

func (a *ResponsesWSSessionActor) dispatchPendingCreateCancel(attemptID string) {
	if a == nil || !a.hasPendingCreateCancel(attemptID) {
		return
	}
	frame := a.turns.pending.cancel.createFrame
	a.clearPendingCreateCancel(attemptID)
	if a.turns.active.attempt == nil || a.turns.active.attempt.AttemptID != strings.TrimSpace(attemptID) {
		return
	}
	if a.upstream.session == nil {
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusConflict, "session_closed", "responses websocket session is not open"))
		return
	}
	if !a.SendProviderControlFrame(attemptID, a.upstream.channelID, a.upstream.session, frame) {
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusServiceUnavailable, "responses_ws_send_queue_full", responsesWSStaticErrorMessage("responses_ws_send_queue_full")))
	}
}

func (a *ResponsesWSSessionActor) releasePendingLease() {
	if a == nil {
		return
	}
	a.lease.mu.Lock()
	lease := a.lease.pendingLease
	a.lease.pendingLease = nil
	a.lease.mu.Unlock()
	if lease != nil {
		lease.Release()
	}
}

func (a *ResponsesWSSessionActor) releaseActiveLease() {
	if a == nil {
		return
	}
	a.lease.mu.Lock()
	lease := a.lease.activeLease
	a.lease.activeLease = nil
	a.lease.mu.Unlock()
	if lease != nil {
		lease.Release()
	}
}

func (a *ResponsesWSSessionActor) setPendingLease(lease middleware.ResponsesWSLease) {
	if a == nil {
		return
	}
	a.lease.mu.Lock()
	a.lease.pendingLease = lease
	a.lease.mu.Unlock()
}

func (a *ResponsesWSSessionActor) setActiveLease(lease middleware.ResponsesWSLease) {
	if a == nil {
		return
	}
	a.lease.mu.Lock()
	a.lease.activeLease = lease
	a.lease.mu.Unlock()
	a.armActiveLeaseLossWatch(lease)
}

func (a *ResponsesWSSessionActor) armActiveLeaseLossWatch(lease middleware.ResponsesWSLease) {
	if a == nil || lease == nil {
		return
	}
	lost := lease.Lost()
	if lost == nil {
		return
	}
	go func() {
		select {
		case <-a.done:
			return
		case <-lost:
			// Trade-off: once the shared Redis lease is lost we close the session instead
			// of silently degrading to a process-local counter, so cluster-wide active
			// limits remain trustworthy under Redis churn.
			if !a.PostReliable(ResponsesWSEventTimeout{Reason: "responses_ws_active_lease_lost"}) {
				return
			}
		}
	}()
}

func (a *ResponsesWSSessionActor) markActivity() {
	if a == nil {
		return
	}
	a.setLastActivity(time.Now())
}

func (a *ResponsesWSSessionActor) setLastActivity(last time.Time) {
	if a == nil {
		return
	}
	if last.IsZero() {
		last = time.Now()
	}
	a.watchdog.lastActivityMu.Lock()
	a.watchdog.lastActivity = last
	a.watchdog.lastActivityMu.Unlock()
}

func (a *ResponsesWSSessionActor) lastActivity() time.Time {
	if a == nil {
		return time.Now()
	}
	a.watchdog.lastActivityMu.Lock()
	last := a.watchdog.lastActivity
	a.watchdog.lastActivityMu.Unlock()
	if last.IsZero() {
		return time.Now()
	}
	return last
}

func (a *ResponsesWSSessionActor) loop() {
	defer a.finish()
	defer recoverResponsesWSGoroutine("actor_loop", func(reason string) {
		a.close(reason)
	})
	for {
		select {
		case <-a.done:
			return
		case event := <-a.events:
			a.handleEvent(event)
			if a.closing.closed.Load() {
				return
			}
		}
	}
}

func (a *ResponsesWSSessionActor) finish() {
	if a == nil {
		return
	}
	a.doneOnce.Do(func() {
		close(a.done)
	})
}

func (a *ResponsesWSSessionActor) idleWatchdog() {
	defer recoverResponsesWSGoroutine("idle_watchdog", func(reason string) {
		if !a.PostReliable(ResponsesWSEventTimeout{Reason: reason}) {
			return
		}
	})
	timeout := config.ResponsesWSIdleTimeout()
	if timeout <= 0 {
		return
	}
	interval := timeout / 4
	if interval <= 0 || interval > responsesWSIdleWatchdogMaxInterval {
		interval = responsesWSIdleWatchdogMaxInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-a.done:
			return
		case <-ticker.C:
			if time.Since(a.lastActivity()) >= timeout {
				if !a.PostReliable(ResponsesWSEventTimeout{Reason: "idle_timeout"}) {
					return
				}
				return
			}
		}
	}
}

func (a *ResponsesWSSessionActor) armActiveTurnWatchdog() {
	if a == nil || a.turns.active.attempt == nil || a.closing.closed.Load() {
		return
	}
	timeout := config.ResponsesWSActiveTurnTimeout()
	if timeout <= 0 {
		return
	}
	attemptID := a.turns.active.attempt.AttemptID
	generation := a.upstream.sessionGeneration
	channelID := a.turns.active.channelID
	a.watchdog.activeTurnMu.Lock()
	a.watchdog.activeTurnTimerGen++
	timerGen := a.watchdog.activeTurnTimerGen
	if a.watchdog.activeTurnTimer != nil {
		a.watchdog.activeTurnTimer.Stop()
	}
	a.watchdog.activeTurnTimer = time.AfterFunc(timeout, func() {
		if !a.PostReliable(ResponsesWSEventTimeout{
			Reason:                    responsesWSActiveTurnTimeoutReason,
			UpstreamSessionGeneration: generation,
			ChannelID:                 channelID,
			AttemptID:                 attemptID,
			TimeoutGeneration:         timerGen,
		}) {
			return
		}
	})
	a.watchdog.activeTurnMu.Unlock()
}

func (a *ResponsesWSSessionActor) stopActiveTurnWatchdog() {
	if a == nil {
		return
	}
	a.watchdog.activeTurnMu.Lock()
	a.watchdog.activeTurnTimerGen++
	if a.watchdog.activeTurnTimer != nil {
		a.watchdog.activeTurnTimer.Stop()
		a.watchdog.activeTurnTimer = nil
	}
	a.watchdog.activeTurnMu.Unlock()
}

func (a *ResponsesWSSessionActor) activeTurnWatchdogGenerationMatches(generation int64) bool {
	if a == nil || generation <= 0 {
		return false
	}
	a.watchdog.activeTurnMu.Lock()
	defer a.watchdog.activeTurnMu.Unlock()
	return generation == a.watchdog.activeTurnTimerGen && a.watchdog.activeTurnTimer != nil
}

func (a *ResponsesWSSessionActor) handleEvent(event ResponsesWSEvent) {
	switch typed := event.(type) {
	case ResponsesWSEventFirstTurnSetup:
		a.handleFirstTurnSetup(typed)
	case ResponsesWSEventFirstTurnOpenResult:
		a.handleFirstTurnOpenResult(typed)
	case ResponsesWSEventClientFrame:
		a.handleClientFrame(typed)
	case ResponsesWSEventSendResult:
		a.handleSendResult(typed)
	case ResponsesWSEventTransportContractViolation:
		a.handleTransportContractViolation(typed)
	case ResponsesWSEventProviderDownstream:
		a.handleProviderDownstream(typed)
	case ResponsesWSEventProviderUsageObserved:
		a.handleProviderUsageObserved(typed)
	case ResponsesWSEventProviderBusinessError:
		a.handleProviderBusinessError(typed)
	case ResponsesWSEventProviderRecvFailed:
		a.handleProviderRecvFailed(typed)
	case ResponsesWSEventProviderClosed:
		a.handleProviderClosed(typed)
	case ResponsesWSEventBridgeOpenProviderError:
		if typed.UpstreamSessionGeneration != "" && typed.UpstreamSessionGeneration != a.upstream.sessionGeneration {
			return
		}
		if typed.ChannelID > 0 && typed.ChannelID != a.upstream.channelID {
			return
		}
		if !a.providerEventAttemptMatches(typed.AttemptID) {
			a.logIgnoredProviderEvent("bridge_open_provider_error_attempt_mismatch", typed.ChannelID, responsesws.RecvDetailOriginBridgeOpenProviderError, typed.DetailPhase)
			return
		}
		if !a.observeBridgeOpenProviderError(typed) {
			return
		}
		a.handleBridgeOpenProviderError(typed)
	case ResponsesWSEventBridgeOpenLocalError:
		if typed.UpstreamSessionGeneration != "" && typed.UpstreamSessionGeneration != a.upstream.sessionGeneration {
			return
		}
		if typed.ChannelID > 0 && typed.ChannelID != a.upstream.channelID {
			return
		}
		if !a.providerEventAttemptMatches(typed.AttemptID) {
			a.logIgnoredProviderEvent("bridge_open_local_error_attempt_mismatch", typed.ChannelID, responsesws.RecvDetailOriginBridgeStreamError, typed.DetailPhase)
			return
		}
		if !a.observeBridgeOpenLocalError(typed) {
			return
		}
		if a.handleBridgeLocalOpenError(typed) {
			return
		}
		a.writeProxyLocal(typed.Payload)
		if !typed.Recoverable {
			a.close("bridge_open_local_error")
		}
	case ResponsesWSEventProxyLocalError:
		if typed.UpstreamSessionGeneration != "" && typed.UpstreamSessionGeneration != a.upstream.sessionGeneration {
			return
		}
		if typed.ChannelID > 0 && typed.ChannelID != a.upstream.channelID {
			return
		}
		if !a.providerEventAttemptMatches(typed.AttemptID) {
			a.logIgnoredProviderEvent("proxy_local_error_attempt_mismatch", typed.ChannelID, typed.DetailOrigin, typed.DetailPhase)
			return
		}
		if !a.observeProxyLocalError(typed) {
			return
		}
		a.writeProxyLocal(typed.Payload)
		if !typed.Recoverable {
			a.close("proxy_local_error")
		}
	case ResponsesWSEventClientClosed:
		a.handleClientClosed(typed.Err)
	case ResponsesWSEventTimeout:
		a.handleTimeout(typed)
	case ResponsesWSEventCloseIntent:
		a.close(typed.Reason)
	default:
		a.logWarnf("responses websocket dropped unknown actor event: event_type=%T", event)
	}
}

func (a *ResponsesWSSessionActor) handleTimeout(event ResponsesWSEventTimeout) {
	if event.UpstreamSessionGeneration != "" && event.UpstreamSessionGeneration != a.upstream.sessionGeneration {
		return
	}
	if event.ChannelID > 0 && event.ChannelID != a.upstream.channelID {
		return
	}
	if event.Reason == responsesWSActiveTurnTimeoutReason {
		a.handleActiveTurnTimeout(event)
		return
	}
	if event.Reason == responsesWSBridgeProviderRejectionWaitTimeoutReason {
		a.handleBridgeProviderRejectionWaitTimeout(event)
		return
	}
	if event.Reason == responsesWSBridgeLocalOpenErrorWaitTimeoutReason {
		a.handleBridgeLocalOpenErrorWaitTimeout(event)
		return
	}
	a.close(event.Reason)
}

func (a *ResponsesWSSessionActor) handleActiveTurnTimeout(event ResponsesWSEventTimeout) {
	if a == nil || a.turns.active.attempt == nil {
		return
	}
	if event.AttemptID == "" || event.AttemptID != a.turns.active.attempt.AttemptID {
		return
	}
	if !a.activeTurnWatchdogGenerationMatches(event.TimeoutGeneration) {
		return
	}
	receivedAt := time.Now()
	a.turns.active.attempt.MarkCompleted(receivedAt)
	if err := a.finalizeActiveAttempt(); err != nil {
		a.handleActiveSettlementFailure(err)
		return
	}
	a.clearActiveTurn()
	a.writeProxyLocal(responsesWSErrorPayload(http.StatusGatewayTimeout, responsesWSActiveTurnTimeoutReason, "upstream responses websocket turn timed out"))
	a.close(responsesWSActiveTurnTimeoutReason)
}

func (a *ResponsesWSSessionActor) scheduleBridgeProviderRejectionWaitTimeout(attemptID string) {
	if a == nil || strings.TrimSpace(attemptID) == "" {
		return
	}
	timeout := responsesWSBridgeProviderRejectionWaitTimeout
	if timeout < 0 {
		return
	}
	if timeout <= 0 {
		timeout = 200 * time.Millisecond
	}
	generation := a.upstream.sessionGeneration
	channelID := a.upstream.channelID
	time.AfterFunc(timeout, func() {
		_ = a.PostReliable(ResponsesWSEventTimeout{
			Reason:                    responsesWSBridgeProviderRejectionWaitTimeoutReason,
			UpstreamSessionGeneration: generation,
			ChannelID:                 channelID,
			AttemptID:                 attemptID,
		})
	})
}

func (a *ResponsesWSSessionActor) handleBridgeProviderRejectionWaitTimeout(event ResponsesWSEventTimeout) {
	if a == nil || a.turns.pending.attempt == nil {
		return
	}
	attempt := a.turns.pending.attempt
	if event.AttemptID == "" || event.AttemptID != attempt.AttemptID {
		return
	}
	if a.pendingBridgeOpenProviderErrorRecorded() {
		return
	}
	if responsesWSTransportSendStatus(attempt.TransportResult) != responsesws.ResponsesWSTransportSendRejectedBeforeStream || attempt.QuotaFinalized {
		return
	}
	a.logErrorf("responses websocket bridge provider rejection event wait timed out: attempt_id=%s", responsesWSSafeDiagnosticValue(event.AttemptID))
	if !attempt.RolledBack {
		input := a.buildPendingSettlementInput("provider_rejected_before_stream_timeout", responsesWSZeroChargeProof(ResponsesWSZeroChargeProofProviderRejectedBeforeStream, "provider_rejected_before_stream_timeout"))
		decision, _, err := a.applyPendingSettlement(input)
		if err != nil {
			a.writeProxyLocal(responsesWSErrorPayload(http.StatusInternalServerError, "quota_rollback_failed", responsesWSStaticErrorMessage("quota_rollback_failed")))
			a.close("quota_rollback_failed")
			return
		}
		if responsesWSSettlementHasFlag(decision, ResponsesWSSettlementFlagContradictoryInput) {
			a.observeSettlementConflict(string(ResponsesWSSettlementFlagContradictoryInput), decision)
		}
	}
	a.clearPendingTurn("provider_rejected_before_stream_timeout")
	a.state = responsesWSStateIdle
	a.writeProxyLocal(responsesWSErrorPayload(http.StatusBadGateway, responsesWSBridgeProviderRejectionFallbackErrorCode, responsesWSBridgeProviderRejectionFallbackErrorReason))
}

func (a *ResponsesWSSessionActor) scheduleBridgeLocalOpenErrorWaitTimeout(attemptID string) {
	if a == nil || strings.TrimSpace(attemptID) == "" {
		return
	}
	timeout := responsesWSBridgeLocalOpenErrorWaitTimeout
	if timeout < 0 {
		return
	}
	if timeout <= 0 {
		timeout = 200 * time.Millisecond
	}
	generation := a.upstream.sessionGeneration
	channelID := a.upstream.channelID
	time.AfterFunc(timeout, func() {
		_ = a.PostReliable(ResponsesWSEventTimeout{
			Reason:                    responsesWSBridgeLocalOpenErrorWaitTimeoutReason,
			UpstreamSessionGeneration: generation,
			ChannelID:                 channelID,
			AttemptID:                 attemptID,
		})
	})
}

func (a *ResponsesWSSessionActor) handleBridgeLocalOpenErrorWaitTimeout(event ResponsesWSEventTimeout) {
	if a == nil || a.turns.pending.attempt == nil {
		return
	}
	attempt := a.turns.pending.attempt
	if event.AttemptID == "" || event.AttemptID != attempt.AttemptID {
		return
	}
	if a.turns.pending.phase != responsesWSPendingTurnSend ||
		a.state != responsesWSStatePendingSend ||
		a.hasPendingProviderEvidence() ||
		!a.pendingBridgeOpenLocalErrorAwaiting() ||
		responsesWSTransportSendStatus(attempt.TransportResult) != responsesws.ResponsesWSTransportSendAmbiguous ||
		attempt.QuotaFinalized {
		return
	}
	a.logErrorf("responses websocket bridge local open error event wait timed out: attempt_id=%s", responsesWSSafeDiagnosticValue(event.AttemptID))
	input := a.buildPendingSettlementInput("bridge_open_local_error_timeout", ResponsesWSZeroChargeProof{})
	_, _, err := a.applyPendingSettlement(input)
	if err != nil {
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusInternalServerError, "quota_settlement_failed", responsesWSStaticErrorMessage("quota_settlement_failed")))
		a.close("quota_settlement_failed")
		return
	}
	a.clearPendingTurn("bridge_open_local_error_timeout")
	a.state = responsesWSStateIdle
	a.writeProxyLocal(responsesWSErrorPayload(http.StatusBadGateway, "ws_request_failed", "upstream responses websocket bridge failed before provider status"))
	a.close("bridge_open_local_error_timeout")
}

func (a *ResponsesWSSessionActor) handleBridgeOpenProviderError(event ResponsesWSEventBridgeOpenProviderError) {
	if a == nil {
		return
	}
	activeProviderRejection := false
	if a.turns.pending.attempt != nil || a.turns.active.attempt != nil {
		attempt := a.turns.pending.attempt
		continuationMiss := responsesWSBridgeOpenProviderContinuationMiss(event)
		if attempt != nil {
			if responsesWSTransportSendStatus(attempt.TransportResult) == "" && event.Recoverable && !a.hasPendingProviderEvidence() {
				a.rememberPendingBridgeOpenProviderError(event)
				return
			}
		} else {
			attempt = a.turns.active.attempt
			activeProviderRejection = attempt != nil
		}
		if !attempt.RolledBack && !attempt.QuotaFinalized {
			if a.turns.pending.attempt != nil {
				input := a.buildPendingSettlementInput("bridge_open_provider_error", responsesWSZeroChargeProof(ResponsesWSZeroChargeProofProviderRejectedBeforeStream, "bridge_open_provider_error"))
				decision, _, err := a.applyPendingSettlement(input)
				if err != nil {
					a.writeProxyLocal(responsesWSErrorPayload(http.StatusInternalServerError, "quota_rollback_failed", responsesWSStaticErrorMessage("quota_rollback_failed")))
					a.close("quota_rollback_failed")
					return
				}
				if responsesWSSettlementHasFlag(decision, ResponsesWSSettlementFlagContradictoryInput) {
					a.observeSettlementConflict(string(ResponsesWSSettlementFlagContradictoryInput), decision)
				}
			} else {
				// Active bridge-open provider errors are stale/protocol-conflict
				// evidence, not before-stream zero-charge proof. We conservatively
				// finish the active attempt by floor/observed evidence and close the
				// session. Trade-off: this can overbill a small floor on exceptional
				// active paths, but it prevents refunding a turn that may have already
				// reached the provider.
				input := a.buildActiveSettlementInput("bridge_open_provider_error", ResponsesWSZeroChargeProof{})
				_, _, err := a.applyActiveSettlement(input)
				if err != nil {
					a.writeProxyLocal(responsesWSErrorPayload(http.StatusInternalServerError, "quota_settlement_failed", responsesWSStaticErrorMessage("quota_settlement_failed")))
					a.close("quota_settlement_failed")
					return
				}
			}
		}
		a.applyBridgeOpenProviderErrorSideEffects(event, attempt, continuationMiss)
		a.clearBridgeOpenProviderTurnState(event.AttemptID)
	} else {
		if event.ProviderAPIError != nil && a.markProviderAPIErrorSeen(event.ProviderAPIError, "responses_ws_bridge_open_provider_error") {
			processProviderAPIError(a.Context(), a.providerPayloadChannel(event.ChannelID), event.ProviderAPIError, "responses_ws_bridge_open_provider_error")
		}
		if event.Payload != nil {
			a.writeProxyLocal(event.Payload)
		}
	}
	if activeProviderRejection || !event.Recoverable {
		a.close("bridge_open_provider_error")
	}
}

func (a *ResponsesWSSessionActor) rememberPendingBridgeOpenProviderError(event ResponsesWSEventBridgeOpenProviderError) {
	if a == nil {
		return
	}
	copied := event
	if event.Payload != nil {
		copied.Payload = append([]byte(nil), event.Payload...)
	}
	a.turns.pending.provider.bridgeOpenProviderErr = &copied
}

func (a *ResponsesWSSessionActor) applyPendingBridgeOpenProviderErrorSideEffects(attempt *ResponsesWSTurnAttempt) {
	if a == nil || a.turns.pending.provider.bridgeOpenProviderErr == nil {
		return
	}
	event := *a.turns.pending.provider.bridgeOpenProviderErr
	a.applyBridgeOpenProviderErrorSideEffects(event, attempt, responsesWSBridgeOpenProviderContinuationMiss(event))
}

func (a *ResponsesWSSessionActor) applyBridgeOpenProviderErrorSideEffects(event ResponsesWSEventBridgeOpenProviderError, attempt *ResponsesWSTurnAttempt, continuationMiss bool) {
	if a == nil {
		return
	}
	if continuationMiss && attempt != nil {
		if a.turns.pending.attempt != nil {
			a.applyContinuationMissSideEffects(attempt.Candidate, attempt.SelectedChannelID, attempt.AttemptedPreviousResponseID)
		} else {
			a.applyContinuationMissSideEffects(a.turns.active.affinity, a.turns.active.channelID, attempt.AttemptedPreviousResponseID)
		}
	}
	if event.ProviderAPIError != nil && a.markProviderAPIErrorSeen(event.ProviderAPIError, "responses_ws_bridge_open_provider_error") {
		processProviderAPIError(a.Context(), a.providerPayloadChannel(event.ChannelID), event.ProviderAPIError, "responses_ws_bridge_open_provider_error")
	}
	if event.Payload != nil {
		a.writeProxyLocal(event.Payload)
	}
}

func (a *ResponsesWSSessionActor) clearBridgeOpenProviderTurnState(attemptID string) {
	if a == nil {
		return
	}
	a.clearPendingTurn("bridge_open_provider_error")
	if err := a.finishActiveTurn("bridge_open_provider_error", ""); err != nil {
		a.logErrorf("responses websocket active finish transition failed: %v", err)
	}
	a.state = responsesWSStateIdle
}

func (a *ResponsesWSSessionActor) handleBridgeLocalOpenError(event ResponsesWSEventBridgeOpenLocalError) bool {
	if a == nil || a.turns.pending.attempt == nil {
		return false
	}
	sendStatus := responsesWSTransportSendStatus(a.turns.pending.attempt.TransportResult)
	sendResultAllowsLocalOpenError := sendStatus == "" ||
		(sendStatus == responsesws.ResponsesWSTransportSendAmbiguous && a.pendingBridgeOpenLocalErrorAwaiting())
	if !sendResultAllowsLocalOpenError ||
		a.turns.pending.phase != responsesWSPendingTurnSend ||
		a.state != responsesWSStatePendingSend ||
		a.hasPendingProviderEvidence() {
		return false
	}
	if event.AttemptID != "" && event.AttemptID != a.turns.pending.attempt.AttemptID {
		return false
	}
	input := a.buildPendingSettlementInput("bridge_open_local_error", ResponsesWSZeroChargeProof{})
	_, _, err := a.applyPendingSettlement(input)
	if err != nil {
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusInternalServerError, "quota_settlement_failed", responsesWSStaticErrorMessage("quota_settlement_failed")))
		a.close("quota_settlement_failed")
		return true
	}
	a.clearPendingTurn("bridge_open_local_error")
	a.state = responsesWSStateIdle
	a.writeProxyLocal(event.Payload)
	if !event.Recoverable {
		a.close("bridge_open_local_error")
	}
	return true
}

func (a *ResponsesWSSessionActor) observeProxyLocalError(event ResponsesWSEventProxyLocalError) bool {
	if a == nil {
		return false
	}
	upstreamEvent := upstreamEventFromProxyLocalError(event)
	if a.turns.pending.attempt != nil {
		return a.appendPendingProviderLifecycle(upstreamEvent)
	}
	if a.turns.active.attempt != nil {
		a.updateActiveProviderEvidence(upstreamEvent)
	}
	return true
}

func (a *ResponsesWSSessionActor) observeBridgeOpenProviderError(event ResponsesWSEventBridgeOpenProviderError) bool {
	if a == nil {
		return false
	}
	upstreamEvent := upstreamEventFromBridgeOpenProviderError(event)
	if a.turns.pending.attempt != nil {
		return a.appendPendingProviderLifecycle(upstreamEvent)
	}
	return true
}

func (a *ResponsesWSSessionActor) observeBridgeOpenLocalError(event ResponsesWSEventBridgeOpenLocalError) bool {
	if a == nil {
		return false
	}
	upstreamEvent := upstreamEventFromBridgeOpenLocalError(event)
	if a.turns.pending.attempt != nil {
		return a.appendPendingProviderLifecycle(upstreamEvent)
	}
	if a.turns.active.attempt != nil {
		a.updateActiveProviderEvidence(upstreamEvent)
	}
	return true
}

func (a *ResponsesWSSessionActor) pendingBridgeOpenLocalErrorAwaiting() bool {
	if a == nil || a.turns.pending.attempt == nil {
		return false
	}
	return a.turns.pending.provider.bridgeOpenLocalErrorAttemptID != "" && a.turns.pending.provider.bridgeOpenLocalErrorAttemptID == a.turns.pending.attempt.AttemptID
}

func (a *ResponsesWSSessionActor) pendingBridgeOpenProviderErrorRecorded() bool {
	return a != nil && a.turns.pending.provider.bridgeOpenProviderErr != nil
}

func (a *ResponsesWSSessionActor) handleFirstTurnSetup(event ResponsesWSEventFirstTurnSetup) {
	if a == nil {
		return
	}
	a.setPendingLease(event.PendingLease)
	if event.Frame == nil {
		a.releasePendingLease()
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusBadRequest, "invalid_event", "response.create frame is required"))
		a.close("first_turn_setup_missing_frame")
		return
	}

	openingID := a.ReserveFirstTurnOpening(event.Frame)
	if !event.ReceivedAt.IsZero() {
		a.turns.opening.startedAt = event.ReceivedAt
	}
	if a.isClientGone() {
		a.releasePendingLease()
		a.close("client_closed_before_first_turn_setup")
		return
	}

	actorCtx := a.Context()
	request := event.Frame.Projection
	prepareResponsesChannelAffinity(actorCtx, &request)
	ensureResponsesWSConnectionSessionID(actorCtx)
	a.RefreshContext(actorCtx)
	admission := a.turns.opening.admission
	if admission == nil {
		admission = NewResponsesWSTurnAdmission()
		a.turns.opening.admission = admission
	}
	if a.isClientGone() {
		a.close("client_closed_before_active_lease")
		return
	}
	activeLease, apiErr := middleware.AcquireResponsesWSActiveLease(actorCtx)
	if apiErr != nil {
		a.writeProxyLocal(responsesWSErrorFromOpenAI(apiErr))
		a.close("active_lease_failed")
		return
	}
	a.setActiveLease(activeLease)

	if apiErr := admission.AllowRPMOnce(func() *types.OpenAIErrorWithStatusCode {
		return middleware.AllowCurrentUserRequest(a.Context())
	}); apiErr != nil {
		a.writeProxyLocal(responsesWSErrorFromOpenAI(apiErr))
		a.close("rpm_allow_failed")
		return
	}
	if a.isClientGone() {
		a.releasePendingLease()
		a.close("client_closed_before_upstream_open")
		return
	}
	a.releasePendingLease()

	a.startFirstTurnOpenWorker(openingID, event.Frame)
}

func (a *ResponsesWSSessionActor) startFirstTurnOpenWorker(openingID string, frame *responsesws.RawResponsesCreateFrame) {
	if a == nil || frame == nil {
		return
	}
	setupCtx, cancel := context.WithCancel(context.Background())
	a.setSetupCancel(cancel)
	actorSnapshot := a.snapshotClone()

	go func() {
		defer cancel()
		var openResult *responsesWSOpenResult
		handedOff := false
		defer recoverResponsesWSGoroutine("first_turn_open_worker", func(reason string) {
			if !handedOff {
				cleanupResponsesWSOpenResult(openResult, reason)
			}
			if !a.PostReliable(ResponsesWSEventTimeout{Reason: reason}) {
				return
			}
		})
		select {
		case <-setupCtx.Done():
			return
		default:
		}

		var apiErr *types.OpenAIErrorWithStatusCode
		actorContext := actorSnapshot.Context()
		openResult, apiErr = openAndPrimeResponsesWSSessionForActor(setupCtx, actorContext, &frame.Projection)
		if setupCtx.Err() != nil {
			cleanupResponsesWSOpenResult(openResult, "first_turn_open_cancelled")
			return
		}

		adopted := make(chan bool, 1)
		event := ResponsesWSEventFirstTurnOpenResult{
			OpeningID:  openingID,
			Snapshot:   NewResponsesWSRequestSnapshot(actorContext),
			OpenResult: openResult,
			Err:        apiErr,
			Adopted:    adopted,
		}
		select {
		case a.events <- event:
			handedOff = true
		case <-setupCtx.Done():
			cleanupResponsesWSOpenResult(openResult, "first_turn_open_cancelled")
			return
		case <-a.done:
			cleanupResponsesWSOpenResult(openResult, "first_turn_open_abandoned")
			return
		}

		select {
		case ok := <-adopted:
			if !ok {
				cleanupResponsesWSOpenResult(openResult, "first_turn_open_not_adopted")
			}
		case <-a.done:
			cleanupResponsesWSOpenResult(openResult, "first_turn_open_abandoned")
		}
	}()
}

func cleanupResponsesWSOpenResult(openResult *responsesWSOpenResult, reason string) {
	if openResult == nil || openResult.Session == nil {
		return
	}
	openResult.Session.Abort(strings.TrimSpace(reason))
}

func (a *ResponsesWSSessionActor) handleFirstTurnOpenResult(event ResponsesWSEventFirstTurnOpenResult) {
	adopted := false
	defer func() {
		if event.Adopted != nil {
			event.Adopted <- adopted
		}
	}()
	if a == nil {
		return
	}
	defer a.clearSetupCancel()
	if event.OpeningID == "" || event.OpeningID != a.turns.opening.openingID || a.turns.pending.phase != responsesWSPendingTurnOpening {
		cleanupResponsesWSOpenResult(event.OpenResult, "stale_first_turn_open_result")
		adopted = true
		return
	}
	if a.isClientGone() {
		cleanupResponsesWSOpenResult(event.OpenResult, "client_closed_during_open")
		adopted = true
		a.close("client_closed_during_open")
		return
	}
	if event.Snapshot != nil {
		a.setSnapshot(event.Snapshot.Clone())
	}
	if event.Err != nil {
		cleanupResponsesWSOpenResult(event.OpenResult, "first_turn_open_failed")
		if openAIErrorCodeString(event.Err.Code, "") == "responses_ws_unsupported_for_channel" {
			a.writeProxyLocal(responsesWSFallbackPayload())
			if a.io.bridge != nil {
				a.markDownstreamCloseSent()
				a.io.bridge.WriteCloseControl(int(wsconn.CloseNormalClosure), "responses_ws_unsupported_for_channel")
			}
		} else {
			a.writeProxyLocal(responsesWSErrorFromOpenAI(event.Err))
		}
		adopted = true
		a.close("open_failed")
		return
	}
	if event.OpenResult == nil || event.OpenResult.Session == nil || event.OpenResult.Channel == nil {
		cleanupResponsesWSOpenResult(event.OpenResult, "invalid_first_turn_open_result")
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusServiceUnavailable, "channel_error", "responses websocket open did not return a channel"))
		adopted = true
		a.close("open_failed")
		return
	}

	adopted = true
	a.prepareAndSendFirstTurn(event.OpenResult)
}

func (a *ResponsesWSSessionActor) prepareAndSendFirstTurn(openResult *responsesWSOpenResult) {
	if a == nil || openResult == nil || openResult.Session == nil || openResult.Channel == nil || a.turns.opening.firstFrame == nil {
		if openResult != nil && openResult.Session != nil {
			openResult.Session.Abort("first_turn_prepare_failed")
		}
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusServiceUnavailable, "channel_error", "responses websocket first turn setup is incomplete"))
		a.close("first_turn_prepare_failed")
		return
	}
	if a.isClientGone() {
		openResult.Session.Abort("client_closed_before_first_turn_prepare")
		a.close("client_closed_before_first_turn_prepare")
		return
	}

	a.mutateSnapshot(func(snapshot *ResponsesWSRequestSnapshot) {
		attachResponsesWSSelectedChannelSnapshot(snapshot, openResult.Channel, openResult.ProviderModel, openResult.BillingModel)
	})
	actorCtx := a.Context()
	actorSnapshot := a.snapshotClone()
	upstreamSessionGeneration := a.AttachUpstreamSession(openResult.Session, openResult.Channel.Id)
	admission := a.turns.opening.admission
	if admission == nil {
		admission = NewResponsesWSTurnAdmission()
		a.turns.opening.admission = admission
	}
	attempt, apiErr := PrepareResponsesWSTurnAttempt(ResponsesWSTurnAttemptInput{
		Context:           actorCtx,
		Snapshot:          actorSnapshot,
		OpeningID:         a.turns.opening.openingID,
		Admission:         admission,
		Candidate:         openResult.Candidate,
		SelectedChannelID: openResult.Channel.Id,
		Session:           openResult.Session,
		BillingModel:      openResult.BillingModel,
		PromptModel:       openResult.ProviderModel,
		Request:           &a.turns.opening.firstFrame.Projection,
		StartedAt:         a.turns.opening.startedAt,
	})
	if apiErr != nil {
		a.writeProxyLocal(responsesWSErrorFromOpenAI(apiErr))
		a.close("attempt_prepare_failed")
		return
	}
	if a.isClientGone() {
		a.close("client_closed_before_attempt_begin")
		return
	}
	if err := a.BeginCandidate(attempt); err != nil {
		a.logErrorf("responses websocket attempt begin failed: %s", err.Error())
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusInternalServerError, "responses_ws_attempt_failed", responsesWSStaticErrorMessage("responses_ws_attempt_failed")))
		a.close("attempt_begin_failed")
		return
	}
	payload, err := responsesWSProviderPayload(actorCtx, a.turns.opening.firstFrame, &a.turns.opening.firstFrame.Projection, openResult.ProviderModel)
	if err != nil {
		if !a.settlePendingAttemptOrClose("rewrite_failed", responsesWSZeroChargeProof(ResponsesWSZeroChargeProofRewriteFailed, "rewrite_failed")) {
			return
		}
		a.logErrorf("responses websocket rewrite failed: %s", err.Error())
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusInternalServerError, "responses_ws_payload_rewrite_failed", responsesWSStaticErrorMessage("responses_ws_payload_rewrite_failed")))
		a.close("rewrite_failed")
		return
	}
	if a.isClientGone() {
		if !a.settlePendingAttemptOrClose("client_closed_before_quota_preconsume", responsesWSZeroChargeProof(ResponsesWSZeroChargeProofClientClosedBeforeSend, "client_closed_before_quota_preconsume")) {
			return
		}
		a.close("client_closed_before_quota_preconsume")
		return
	}
	if apiErr := attempt.PreConsumeQuota(); apiErr != nil {
		if !a.settlePendingAttemptOrClose("quota_preconsume_failed", responsesWSZeroChargeProof(ResponsesWSZeroChargeProofQuotaRejected, "quota_preconsume_failed")) {
			return
		}
		a.writeProxyLocal(responsesWSErrorFromOpenAI(apiErr))
		a.close("quota_preconsume_failed")
		return
	}
	if a.isClientGone() {
		if !a.settlePendingAttemptOrClose("client_closed_before_provider_send", responsesWSZeroChargeProof(ResponsesWSZeroChargeProofClientClosedBeforeSend, "client_closed_before_provider_send")) {
			return
		}
		a.close("client_closed_before_provider_send")
		return
	}

	a.MarkPendingSend()
	a.upstream.recvArmed = true
	a.io.bridge.ArmProviderRecvPump(upstreamSessionGeneration, openResult.Channel.Id, openResult.Session)
	if !a.SendProviderFrame(attempt.AttemptID, openResult.Channel.Id, openResult.Session, responsesws.NewTextFrame(payload)) {
		a.handleSendQueueFull(attempt.AttemptID, openResult.Channel.Id)
	}
}

func (a *ResponsesWSSessionActor) handleClientFrame(event ResponsesWSEventClientFrame) {
	if event.Frame.Kind() != responsesws.FrameKindText {
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusBadRequest, "invalid_event", "only text websocket events are supported"))
		return
	}
	payload := event.Frame.Payload()
	envelope, err := responsesws.ParseClientEventEnvelope(payload)
	if err != nil {
		a.logErrorf("responses websocket client frame parse failed: %s", err.Error())
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusBadRequest, "invalid_event", responsesWSMessageInvalidWebsocketEvent))
		return
	}
	switch strings.TrimSpace(envelope.Type) {
	case "response.create":
		if a.isBusy() {
			if a.recordBusyReject() {
				a.writeProxyLocal(responsesWSErrorPayload(http.StatusTooManyRequests, "responses_ws_busy_rate_limited", "too many response.create frames while the session is busy"))
				a.close("responses_ws_busy_rate_limited")
				return
			}
			a.writeProxyLocal(responsesWSErrorPayload(http.StatusConflict, "session_busy", "responses websocket session already has an inflight response"))
			return
		}
		a.resetBusyRejects()
		a.startSubsequentTurn(payload, event.ReceivedAt)
	case "response.cancel":
		a.handleClientCancel(event)
	default:
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusBadRequest, "unsupported_client_event", "unsupported responses websocket client event"))
	}
}

func (a *ResponsesWSSessionActor) handleClientCancel(event ResponsesWSEventClientFrame) {
	if a == nil {
		return
	}
	if a.turns.pending.phase == responsesWSPendingTurnOpening || (a.state == responsesWSStateOpening && a.upstream.session == nil) {
		a.cancelSetup()
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusConflict, "session_closed", "responses websocket session is not open"))
		a.close("response_cancel_during_opening")
		return
	}
	if a.turns.pending.phase == responsesWSPendingTurnPrepare {
		if !a.settlePendingAttemptOrClose("response_cancel_before_provider_send", responsesWSZeroChargeProof(ResponsesWSZeroChargeProofClientClosedBeforeSend, "response_cancel_before_provider_send")) {
			return
		}
		return
	}
	if a.turns.pending.attempt != nil && (a.turns.pending.phase == responsesWSPendingTurnSend || a.state == responsesWSStatePendingSend) {
		attemptID := a.turns.pending.attempt.AttemptID
		a.markPendingCreateCancel(attemptID, event)
		a.cancelPendingSend(attemptID)
		if a.upstream.session == nil {
			a.writeProxyLocal(responsesWSErrorPayload(http.StatusConflict, "session_closed", "responses websocket session is not open"))
			return
		}
		if !a.SendProviderControlFrame(attemptID, a.upstream.channelID, a.upstream.session, event.Frame) {
			a.writeProxyLocal(responsesWSErrorPayload(http.StatusServiceUnavailable, "responses_ws_send_queue_full", responsesWSStaticErrorMessage("responses_ws_send_queue_full")))
		}
		return
	}
	hasProviderTurn := a.turns.active.attempt != nil ||
		a.turns.pending.attempt != nil ||
		a.turns.pending.phase == responsesWSPendingTurnSend ||
		a.state == responsesWSStatePendingSend ||
		a.state == responsesWSStateInFlight
	if !hasProviderTurn {
		return
	}
	if a.upstream.session == nil {
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusConflict, "session_closed", "responses websocket session is not open"))
		return
	}
	attemptID := ""
	if a.turns.active.attempt != nil {
		attemptID = a.turns.active.attempt.AttemptID
	} else if a.turns.pending.attempt != nil {
		attemptID = a.turns.pending.attempt.AttemptID
	}
	if !a.SendProviderControlFrame(attemptID, a.upstream.channelID, a.upstream.session, event.Frame) {
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusServiceUnavailable, "responses_ws_send_queue_full", responsesWSStaticErrorMessage("responses_ws_send_queue_full")))
	}
}

func (a *ResponsesWSSessionActor) onClientFrame(ctx context.Context, mt wsconn.MessageType, payload []byte) {
	if a == nil {
		return
	}
	select {
	case <-a.done:
		return
	default:
	}
	a.markActivity()
	event := ResponsesWSEventClientFrame{
		Frame:      responsesWSFrameFromWireMessage(int(mt), payload),
		ReceivedAt: time.Now(),
	}
	select {
	case a.events <- event:
	case <-a.done:
		return
	default:
		a.requestCloseIntent("client_frame_backpressure")
		if a.io.client != nil {
			a.io.client.Close(wsconn.CloseInfo{
				Kind:   wsconn.CloseKindBackpressure,
				Code:   wsconn.CloseTryAgainLater,
				Reason: "client_frame_backpressure",
				Err:    errResponsesWSClientFrameBackpressure,
			})
		}
	}
	_ = ctx
}

func (a *ResponsesWSSessionActor) onClientConnClosed(info wsconn.CloseInfo) {
	if a == nil {
		return
	}
	go func() {
		defer recoverResponsesWSGoroutine("client_close_post", nil)
		if !a.PostReliable(ResponsesWSEventClientClosed{Err: info.Err}) {
			return
		}
	}()
}

func (a *ResponsesWSSessionActor) recordBusyReject() bool {
	if a == nil {
		return true
	}
	now := time.Now()
	if a.watchdog.busyRejectWindowStart.IsZero() || now.Sub(a.watchdog.busyRejectWindowStart) > responsesWSBusyRejectWindow {
		a.watchdog.busyRejectWindowStart = now
		a.watchdog.busyRejects = 0
	}
	a.watchdog.busyRejects++
	return a.watchdog.busyRejects > responsesWSBusyRejectLimit
}

func (a *ResponsesWSSessionActor) resetBusyRejects() {
	if a == nil {
		return
	}
	a.watchdog.busyRejectWindowStart = time.Time{}
	a.watchdog.busyRejects = 0
}

func (a *ResponsesWSSessionActor) startSubsequentTurn(raw []byte, receivedAt time.Time) {
	frame, err := responsesws.ParseRawResponsesCreateFrame(raw)
	if err != nil {
		a.logErrorf("responses websocket subsequent frame parse failed: %s", err.Error())
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusBadRequest, responsesWSErrorCodeInvalidResponseCreate, responsesWSMessageInvalidResponseCreate))
		return
	}
	ctx := a.Context()
	lockedSessionModel := strings.TrimSpace(ctx.GetString("original_model"))
	if mismatch := responsesWSSubsequentModelMismatch(frame.Projection.Model, lockedSessionModel); mismatch != "" {
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusConflict, "responses_ws_model_mismatch", mismatch))
		return
	}
	request := frame.Projection
	request.Model = lockedSessionModel
	if request.Model == "" {
		request.Model = frame.Projection.Model
	}
	providerModel, billingModel := responsesWSCurrentModelNames(ctx)
	candidate, err := PrepareResponsesTurnAffinity(ResponsesAffinityInput{Context: ctx, Request: &request})
	if err != nil {
		a.logErrorf("responses websocket affinity conflict: %s", err.Error())
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusConflict, "responses_affinity_conflict", responsesWSStaticErrorMessage("responses_affinity_conflict")))
		return
	}
	if err := responsesAffinityOwnerConflict(candidate, a.upstream.channelID); err != nil {
		a.logErrorf("responses websocket affinity owner conflict: %s", err.Error())
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusConflict, "responses_affinity_conflict", responsesWSStaticErrorMessage("responses_affinity_conflict")))
		return
	}

	channel, _ := ctx.Get("responses_ws_selected_channel")
	if typed, ok := channel.(*model.Channel); ok && typed != nil {
		ctx.Set("channel_id", typed.Id)
		ctx.Set("channel_type", typed.Type)
		a.RefreshContext(ctx)
		ctx = a.Context()
	}
	if err := a.preflightResponsesWSSend(ctx, frame.EventID, &request); err != nil {
		a.handleResponsesWSPreflightError(err)
		return
	}
	admission := NewResponsesWSTurnAdmission()
	if apiErr := admission.AllowRPMOnce(func() *types.OpenAIErrorWithStatusCode {
		return middleware.AllowCurrentUserRequest(ctx)
	}); apiErr != nil {
		a.writeProxyLocal(responsesWSErrorFromOpenAI(apiErr))
		return
	}
	attempt, apiErr := PrepareResponsesWSTurnAttempt(ResponsesWSTurnAttemptInput{
		Context:           ctx,
		Snapshot:          a.snapshotClone(),
		OpeningID:         "",
		Admission:         admission,
		Candidate:         candidate,
		SelectedChannelID: a.upstream.channelID,
		Session:           a.upstream.session,
		BillingModel:      billingModel,
		PromptModel:       providerModel,
		Request:           &request,
		StartedAt:         receivedAt,
	})
	if apiErr != nil {
		a.writeProxyLocal(responsesWSErrorFromOpenAI(apiErr))
		return
	}
	if err := a.BeginCandidate(attempt); err != nil {
		a.logErrorf("responses websocket attempt begin failed: %s", err.Error())
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusInternalServerError, "responses_ws_attempt_failed", responsesWSStaticErrorMessage("responses_ws_attempt_failed")))
		return
	}
	payload, err := responsesWSProviderPayload(ctx, frame, &request, providerModel)
	if err != nil {
		if !a.settlePendingAttemptOrClose("rewrite_failed", responsesWSZeroChargeProof(ResponsesWSZeroChargeProofRewriteFailed, "rewrite_failed")) {
			return
		}
		a.logErrorf("responses websocket rewrite failed: %s", err.Error())
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusInternalServerError, "responses_ws_payload_rewrite_failed", responsesWSStaticErrorMessage("responses_ws_payload_rewrite_failed")))
		return
	}
	if apiErr := attempt.PreConsumeQuota(); apiErr != nil {
		if !a.settlePendingAttemptOrClose("quota_preconsume_failed", responsesWSZeroChargeProof(ResponsesWSZeroChargeProofQuotaRejected, "quota_preconsume_failed")) {
			return
		}
		a.writeProxyLocal(responsesWSErrorFromOpenAI(apiErr))
		return
	}
	a.MarkPendingSend()
	if !a.upstream.recvArmed {
		a.upstream.recvArmed = true
		a.io.bridge.ArmProviderRecvPump(a.upstream.sessionGeneration, a.upstream.channelID, a.upstream.session)
	}
	if !a.SendProviderFrame(attempt.AttemptID, a.upstream.channelID, a.upstream.session, responsesws.NewTextFrame(payload)) {
		a.handleSendQueueFull(attempt.AttemptID, a.upstream.channelID)
	}
}

func (a *ResponsesWSSessionActor) preflightResponsesWSSend(c *gin.Context, eventID string, request *types.OpenAIResponsesRequest) error {
	if a == nil || a.upstream.session == nil || request == nil {
		return nil
	}
	preflight, ok := a.upstream.session.(responsesws.SendPreflightCapable)
	if !ok {
		return nil
	}
	ctx := context.Background()
	if c != nil && c.Request != nil {
		ctx = c.Request.Context()
	}
	return preflight.PreflightResponsesWSSend(ctx, eventID, request)
}

func (a *ResponsesWSSessionActor) handleResponsesWSPreflightError(err error) {
	if err == nil {
		return
	}
	a.writeProxyLocal(responsesWSErrorFromErr(err))
	if errors.Is(err, responsesws.ErrStaleContinuation) {
		a.close("previous_response_not_found")
		return
	}
	a.close("responses_ws_preflight_failed")
}

func (a *ResponsesWSSessionActor) handleSendQueueFull(attemptID string, selectedChannelID int) {
	if a == nil {
		return
	}
	// The actor learns this NotSent outcome synchronously while trying to place
	// the provider send command. Applying it inline avoids a queued client-close
	// event settling a still-unknown pending send and preserving preconsume for
	// bytes that never reached the upstream writer.
	a.handleSendResult(ResponsesWSEventSendResult{
		AttemptID:                 attemptID,
		UpstreamSessionGeneration: a.upstream.sessionGeneration,
		SelectedChannelID:         selectedChannelID,
		Purpose:                   responsesWSSendPurposeFromAttempt(attemptID),
		TransportResult: responsesws.ResponsesWSTransportSendResult{
			Status: responsesws.ResponsesWSTransportSendNotAttempted,
			Err:    errResponsesWSSendQueueFull,
		},
	})
}

func (a *ResponsesWSSessionActor) handleSendResult(event ResponsesWSEventSendResult) {
	if err := responsesws.ValidateResponsesWSTransportSendResult(event.TransportResult); err != nil {
		if event.TransportResult.Err != nil {
			err = errors.Join(event.TransportResult.Err, err)
		}
		a.handleTransportContractViolation(ResponsesWSEventTransportContractViolation{
			AttemptID:                 event.AttemptID,
			ResponseID:                event.ResponseID,
			UpstreamSessionGeneration: event.UpstreamSessionGeneration,
			SelectedChannelID:         event.SelectedChannelID,
			Purpose:                   event.Purpose,
			TransportResult:           event.TransportResult,
			Err:                       err,
		})
		return
	}
	sendErr := event.TransportResult.Err
	if event.AttemptID == "" {
		if sendErr != nil {
			a.writeProxyLocal(responsesWSErrorFromErr(sendErr))
		}
		return
	}
	if event.Purpose != "" && event.Purpose != ResponsesWSSendPurposeResponseCreate {
		if sendErr != nil {
			a.writeProxyLocal(responsesWSErrorFromErr(sendErr))
		}
		a.logIgnoredSendResult(event, "non_response_create_send_result")
		return
	}
	if event.UpstreamSessionGeneration != "" && event.UpstreamSessionGeneration != a.upstream.sessionGeneration {
		a.logIgnoredSendResult(event, "stale_generation_send_result")
		return
	}
	attempt := a.turns.pending.attempt
	if attempt == nil || attempt.AttemptID != event.AttemptID || attempt.SelectedChannelID != event.SelectedChannelID {
		a.logIgnoredSendResult(event, "stale_attempt_send_result")
		return
	}
	pendingCreateCancel := a.hasPendingCreateCancel(event.AttemptID)
	a.clearPendingSendCancel(event.AttemptID)
	attempt.TransportResult = event.TransportResult
	status := responsesWSTransportSendStatus(event.TransportResult)

	switch status {
	case responsesws.ResponsesWSTransportSendAttempted:
		attempt.CommitLocalWriteOK()
		a.commitPendingAttempt(attempt)
		if pendingCreateCancel {
			a.dispatchPendingCreateCancel(event.AttemptID)
		}
	case responsesws.ResponsesWSTransportSendNotAttempted:
		hadProviderEvidence := a.hasPendingProviderEvidence()
		continuationMiss := isProviderReportedContinuationMiss(sendErr)
		input := a.buildPendingSettlementInput("send_not_sent", responsesWSZeroChargeProof(ResponsesWSZeroChargeProofTransportNotAttempted, "send_not_sent"))
		decision, _, err := a.applyPendingSettlement(input)
		if err != nil {
			code := "quota_rollback_failed"
			if hadProviderEvidence {
				code = "quota_settlement_failed"
			}
			a.writeProxyLocal(responsesWSErrorPayload(http.StatusInternalServerError, code, responsesWSStaticErrorMessage(code)))
			a.close(code)
			return
		}
		if hadProviderEvidence || responsesWSSettlementHasFlag(decision, ResponsesWSSettlementFlagContradictoryInput) {
			a.observeSettlementConflict("not_sent_with_provider_evidence", decision)
		}
		if !hadProviderEvidence && continuationMiss {
			a.applyContinuationMissSideEffects(attempt.Candidate, attempt.SelectedChannelID, attempt.AttemptedPreviousResponseID)
		}
		if pendingCreateCancel {
			a.clearPendingTurn("send_not_sent")
			a.state = responsesWSStateIdle
			return
		}
		if hadProviderEvidence {
			a.clearPendingTurn("send_not_sent")
			a.state = responsesWSStateIdle
			if sendErr != nil {
				a.writeProxyLocal(responsesWSErrorFromErr(sendErr))
				if isResponsesWSBridgeOpenProviderError(sendErr) {
					a.close("bridge_open_provider_error")
					return
				}
				if errors.Is(sendErr, responsesws.ErrStaleContinuation) {
					a.close("previous_response_not_found")
					return
				}
				if errors.Is(sendErr, responsesws.ErrUpstreamClosed) {
					a.close("upstream_closed_before_send")
				}
			}
			return
		}
		if a.retryFirstTurnAfterNotSent(attempt) {
			return
		}
		a.clearPendingTurn("send_not_sent")
		a.state = responsesWSStateIdle
		if sendErr != nil {
			a.writeProxyLocal(responsesWSErrorFromErr(sendErr))
			if isResponsesWSBridgeOpenProviderError(sendErr) {
				a.close("bridge_open_provider_error")
				return
			}
			if errors.Is(sendErr, responsesws.ErrStaleContinuation) {
				a.close("previous_response_not_found")
				return
			}
			// Keep attempt-correlated provider evidence strict, but treat a
			// NotSent ErrUpstreamClosed as authoritative session liveness. Leaving
			// the actor idle here would keep routing future turns to a dead native
			// upstream after an ignored stale-attempt close event.
			if errors.Is(sendErr, responsesws.ErrUpstreamClosed) {
				a.close("upstream_closed_before_send")
			}
		}
	case responsesws.ResponsesWSTransportSendRejectedBeforeStream:
		attempt.TransportResult = event.TransportResult
		// Bridge open rejection is produced synchronously by the HTTP bridge, but
		// its recv-pump event races with this send result. Keep the attempt long
		// enough for the queued bridge_open_provider_error to match by AttemptID,
		// with a bounded fallback so a lost/misclassified bridge event cannot
		// leave a settled attempt blocking the actor indefinitely.
		if !a.pendingBridgeOpenProviderErrorRecorded() {
			a.scheduleBridgeProviderRejectionWaitTimeout(event.AttemptID)
			return
		}
		input := a.buildPendingSettlementInput("provider_rejected_before_stream_cleanup", responsesWSZeroChargeProof(ResponsesWSZeroChargeProofProviderRejectedBeforeStream, "provider_rejected_before_stream_cleanup"))
		decision, _, err := a.applyPendingSettlement(input)
		if err != nil {
			a.writeProxyLocal(responsesWSErrorPayload(http.StatusInternalServerError, "quota_rollback_failed", responsesWSStaticErrorMessage("quota_rollback_failed")))
			a.close("quota_rollback_failed")
			return
		}
		if responsesWSSettlementHasFlag(decision, ResponsesWSSettlementFlagContradictoryInput) {
			a.observeSettlementConflict(string(ResponsesWSSettlementFlagContradictoryInput), decision)
		}
		a.applyPendingBridgeOpenProviderErrorSideEffects(attempt)
		a.clearPendingTurn("provider_rejected_before_stream_cleanup")
		a.state = responsesWSStateIdle
	case responsesws.ResponsesWSTransportSendAmbiguous:
		hadProviderEvidence := a.hasPendingProviderEvidence()
		if !hadProviderEvidence {
			// HTTP bridge local-open failures enqueue a precise proxy-local
			// payload before returning ambiguous. The recv-pump event can still
			// arrive after this send result, so keep attempt state long enough
			// for the AttemptID-correlated bridge_stream_error to deliver it.
			if responsesws.IsBridgeOpenLocalError(sendErr) &&
				a.turns.pending.phase == responsesWSPendingTurnSend &&
				a.state == responsesWSStatePendingSend {
				a.turns.pending.provider.bridgeOpenLocalErrorAttemptID = event.AttemptID
				a.scheduleBridgeLocalOpenErrorWaitTimeout(event.AttemptID)
				return
			}
			a.logAmbiguousSendNoProviderEvidence(event)
			input := a.buildPendingSettlementInput("send_ambiguous_no_provider_evidence", ResponsesWSZeroChargeProof{})
			_, _, err := a.applyPendingSettlement(input)
			if err != nil {
				a.writeProxyLocal(responsesWSErrorPayload(http.StatusInternalServerError, "quota_settlement_failed", responsesWSStaticErrorMessage("quota_settlement_failed")))
				a.close("quota_settlement_failed")
				return
			}
			a.clearPendingTurn("send_ambiguous_no_provider_evidence")
			a.state = responsesWSStateIdle
			if pendingCreateCancel || errors.Is(sendErr, responsesws.ErrBridgeOpenCancelled) {
				return
			}
			a.writeProxyLocal(responsesWSErrorPayload(http.StatusBadGateway, "ambiguous_close_no_provider_evidence", "upstream write result is ambiguous without provider evidence"))
			a.close("ambiguous_close_no_provider_evidence")
			return
		}
		attempt.CommitAmbiguousAdmission("send_ambiguous")
		a.commitPendingAttempt(attempt)
		if pendingCreateCancel {
			a.dispatchPendingCreateCancel(event.AttemptID)
		}
	default:
		a.failClosed("responses_ws_unknown_send_result")
	}
}

func (a *ResponsesWSSessionActor) handleTransportContractViolation(event ResponsesWSEventTransportContractViolation) {
	if a == nil {
		return
	}
	err := event.Err
	if err == nil {
		err = responsesws.ErrInvalidResponsesWSTransportSendResult
	}
	a.logErrorf(
		"responses websocket transport contract violation: attempt_id=%s response_id=%s channel_id=%d generation=%s purpose=%s status=%s reason=%s err=%s",
		responsesWSSafeDiagnosticValue(event.AttemptID),
		responsesWSSafeDiagnosticValue(event.ResponseID),
		event.SelectedChannelID,
		responsesWSSafeDiagnosticValue(event.UpstreamSessionGeneration),
		responsesWSSafeDiagnosticValue(string(event.Purpose)),
		responsesWSSafeDiagnosticValue(string(event.TransportResult.Status)),
		responsesWSSafeDiagnosticValue(string(event.TransportResult.Reason)),
		responsesWSSafeErrorDiagnostic(err),
	)
	if event.AttemptID == "" ||
		(event.Purpose != "" && event.Purpose != ResponsesWSSendPurposeResponseCreate) ||
		(event.UpstreamSessionGeneration != "" && event.UpstreamSessionGeneration != a.upstream.sessionGeneration) {
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusBadGateway, "responses_ws_transport_contract_violation", "upstream transport returned an invalid send result"))
		a.close("responses_ws_transport_contract_violation")
		return
	}
	attempt := a.turns.pending.attempt
	if attempt == nil || attempt.AttemptID != event.AttemptID || attempt.SelectedChannelID != event.SelectedChannelID {
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusBadGateway, "responses_ws_transport_contract_violation", "upstream transport returned an invalid send result"))
		a.close("responses_ws_transport_contract_violation")
		return
	}
	attempt.TransportResult = event.TransportResult
	input := a.buildPendingSettlementInput("transport_contract_violation", ResponsesWSZeroChargeProof{})
	if _, _, settleErr := a.applyPendingSettlement(input); settleErr != nil {
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusInternalServerError, "quota_settlement_failed", responsesWSStaticErrorMessage("quota_settlement_failed")))
		a.close("quota_settlement_failed")
		return
	}
	a.clearPendingTurn("transport_contract_violation")
	a.state = responsesWSStateIdle
	a.failClosed("responses_ws_transport_contract_violation")
}

func (a *ResponsesWSSessionActor) logAmbiguousSendNoProviderEvidence(event ResponsesWSEventSendResult) {
	if a == nil {
		return
	}
	logger.LogError(a.logContext(), fmt.Sprintf(
		"responses websocket ambiguous send without provider evidence: attempt_id=%s channel_id=%d generation=%s purpose=%s status=%s reason=%s err=%s",
		responsesWSSafeDiagnosticValue(event.AttemptID),
		event.SelectedChannelID,
		responsesWSSafeDiagnosticValue(event.UpstreamSessionGeneration),
		responsesWSSafeDiagnosticValue(string(event.Purpose)),
		responsesWSSafeDiagnosticValue(string(event.TransportResult.Status)),
		responsesWSSafeDiagnosticValue(string(event.TransportResult.Reason)),
		responsesWSSafeErrorDiagnostic(event.TransportResult.Err),
	))
}

func (a *ResponsesWSSessionActor) logIgnoredSendResult(event ResponsesWSEventSendResult, reason string) {
	if a == nil {
		return
	}
	logger.LogDebug(a.logContext(), fmt.Sprintf(
		"responses websocket ignored send result: reason=%s attempt_id=%s response_id=%s purpose=%s generation=%s current_generation=%s selected_channel_id=%d status=%s",
		reason,
		event.AttemptID,
		event.ResponseID,
		event.Purpose,
		event.UpstreamSessionGeneration,
		a.upstream.sessionGeneration,
		event.SelectedChannelID,
		event.TransportResult.Status,
	))
}

func (a *ResponsesWSSessionActor) logIgnoredProviderEvent(reason string, channelID int, detailOrigin responsesws.RecvDetailOrigin, detailPhase responsesws.RecvDetailPhase) {
	if a == nil {
		return
	}
	logger.LogDebug(a.logContext(), fmt.Sprintf(
		"responses websocket ignored provider event: reason=%s channel_id=%d detail_origin=%s detail_phase=%s current_generation=%s",
		reason,
		channelID,
		detailOrigin,
		detailPhase,
		a.upstream.sessionGeneration,
	))
}

type responsesWSProviderResponseIDDecision int

const (
	responsesWSProviderResponseIDAccepted responsesWSProviderResponseIDDecision = iota
	responsesWSProviderResponseIDStaleFinalized
	responsesWSProviderResponseIDConflict
)

func (a *ResponsesWSSessionActor) checkProviderResponseID(attempt *ResponsesWSTurnAttempt, responseID string) responsesWSProviderResponseIDDecision {
	responseID = strings.TrimSpace(responseID)
	if a == nil || attempt == nil || responseID == "" {
		return responsesWSProviderResponseIDAccepted
	}
	if a.isRecentlyFinalizedResponseID(responseID) {
		return responsesWSProviderResponseIDStaleFinalized
	}
	if !attempt.RememberProviderResponseID(responseID) {
		return responsesWSProviderResponseIDConflict
	}
	return responsesWSProviderResponseIDAccepted
}

func (a *ResponsesWSSessionActor) isRecentlyFinalizedResponseID(responseID string) bool {
	responseID = strings.TrimSpace(responseID)
	if a == nil || responseID == "" {
		return false
	}
	for _, current := range a.turns.history.recentFinalizedResponseIDs {
		if current == responseID {
			return true
		}
	}
	return false
}

func (a *ResponsesWSSessionActor) rememberFinalizedResponseID(responseID string) {
	responseID = strings.TrimSpace(responseID)
	if a == nil || responseID == "" || a.isRecentlyFinalizedResponseID(responseID) {
		return
	}
	a.turns.history.recentFinalizedResponseIDs = append(a.turns.history.recentFinalizedResponseIDs, responseID)
	if len(a.turns.history.recentFinalizedResponseIDs) > responsesWSRecentResponseIDLimit {
		a.turns.history.recentFinalizedResponseIDs = append([]string(nil), a.turns.history.recentFinalizedResponseIDs[len(a.turns.history.recentFinalizedResponseIDs)-responsesWSRecentResponseIDLimit:]...)
	}
}

func responsesWSProviderDownstreamResponseID(event ResponsesWSEventProviderDownstream) string {
	if responseID := strings.TrimSpace(event.ResponseID); responseID != "" {
		return responseID
	}
	if event.Usage != nil {
		if responseID := strings.TrimSpace(event.Usage.ResponseID); responseID != "" {
			return responseID
		}
	}
	if event.Frame != nil && event.Frame.Kind() == responsesws.FrameKindText {
		payload := event.Frame.Payload()
		if len(payload) > 0 {
			return responsesWSPayloadResponseID(payload)
		}
	}
	return ""
}

func responsesWSProviderDownstreamPayload(event ResponsesWSEventProviderDownstream) []byte {
	if event.Frame == nil {
		return nil
	}
	return event.Frame.Payload()
}

func responsesWSProviderUsageResponseID(event ResponsesWSEventProviderUsageObserved) string {
	if responseID := strings.TrimSpace(event.ResponseID); responseID != "" {
		return responseID
	}
	if event.Usage != nil {
		return strings.TrimSpace(event.Usage.ResponseID)
	}
	return ""
}

func responsesWSPayloadResponseID(payload []byte) string {
	var object map[string]json.RawMessage
	if len(payload) == 0 || json.Unmarshal(payload, &object) != nil {
		return ""
	}
	if rawResponseID, ok := object["response_id"]; ok {
		var responseID string
		if json.Unmarshal(rawResponseID, &responseID) == nil {
			if responseID = strings.TrimSpace(responseID); responseID != "" {
				return responseID
			}
		}
	}
	if rawResponse, ok := object["response"]; ok {
		var response struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(rawResponse, &response) == nil {
			return strings.TrimSpace(response.ID)
		}
	}
	return ""
}

func (a *ResponsesWSSessionActor) retryFirstTurnAfterNotSent(previous *ResponsesWSTurnAttempt) bool {
	if a == nil || previous == nil || previous.OpeningID == "" || a.turns.pending.phase == responsesWSPendingTurnNone || a.turns.opening.firstFrame == nil || previous.Admission == nil {
		return false
	}
	if previous.Candidate != nil && previous.Candidate.ExplicitPinID > 0 {
		return false
	}
	ctx := a.Context()
	if currentChannelAffinityStrict(ctx) {
		return false
	}

	failedChannelID := previous.SelectedChannelID
	if failedChannelID > 0 {
		(&relayBase{c: ctx}).skipChannelID(failedChannelID)
		a.RefreshContext(ctx)
	}
	if a.upstream.session != nil && a.io.bridge != nil {
		a.io.bridge.AbortSession(a.upstream.session, "send_not_sent_retry")
	}
	a.upstream.session = nil
	a.upstream.sessionGeneration = ""
	a.upstream.channelID = 0
	a.upstream.recvArmed = false
	a.clearPendingProviderState("send_not_sent_retry")
	a.turns.opening.admission = previous.Admission
	a.mutateSnapshot(clearResponsesWSSelectedChannelSnapshot)

	if a.isClientGone() {
		a.close("client_closed_before_first_turn_retry")
		return true
	}
	a.turns.pending.phase = responsesWSPendingTurnOpening
	a.state = responsesWSStateOpening
	a.startFirstTurnOpenWorker(a.turns.opening.openingID, a.turns.opening.firstFrame)
	return true
}

func (a *ResponsesWSSessionActor) commitPendingAttempt(attempt *ResponsesWSTurnAttempt) {
	a.clearPendingSendCancel(attempt.AttemptID)
	_, replay, err := a.turns.CommitPendingToActive(attempt.SelectedChannelID)
	if err != nil {
		a.logErrorf("responses websocket pending commit transition failed: %v", err)
		a.failClosed("responses_ws_pending_commit_failed")
		return
	}
	a.state = responsesWSStateInFlight
	a.armActiveTurnWatchdog()
	for _, entry := range replay {
		if entry.Downstream != nil {
			a.handleProviderDownstreamReplayed(*entry.Downstream)
			if a.closing.closed.Load() {
				return
			}
		}
		if entry.Failure != nil {
			a.handleProviderRecvFailedReplayed(*entry.Failure)
			if a.closing.closed.Load() {
				return
			}
		}
	}
}

func (a *ResponsesWSSessionActor) handleProviderDownstream(event ResponsesWSEventProviderDownstream) {
	a.handleProviderDownstreamWithObservation(event, true)
}

func (a *ResponsesWSSessionActor) handleProviderDownstreamReplayed(event ResponsesWSEventProviderDownstream) {
	a.handleProviderDownstreamWithObservation(event, false)
}

func (a *ResponsesWSSessionActor) handleProviderDownstreamWithObservation(event ResponsesWSEventProviderDownstream, observe bool) {
	if event.UpstreamSessionGeneration == "" && a.upstream.sessionGeneration != "" {
		a.logIgnoredProviderEvent("provider_downstream_missing_generation", event.ChannelID, event.DetailOrigin, event.DetailPhase)
		return
	}
	if event.UpstreamSessionGeneration != "" && event.UpstreamSessionGeneration != a.upstream.sessionGeneration {
		return
	}
	if event.ChannelID > 0 && event.ChannelID != a.upstream.channelID {
		a.failClosed("responses_ws_provider_channel_mismatch")
		return
	}
	if event.Kind == ProviderDownstreamClose {
		if !a.providerSessionLifecycleAttemptMatches(event.AttemptID) {
			a.logIgnoredProviderEvent("provider_downstream_attempt_mismatch", event.ChannelID, event.DetailOrigin, event.DetailPhase)
			return
		}
	} else if !a.providerEventAttemptMatches(event.AttemptID) {
		a.logIgnoredProviderEvent("provider_downstream_attempt_mismatch", event.ChannelID, event.DetailOrigin, event.DetailPhase)
		return
	}
	upstreamEvent := upstreamEventFromProviderDownstream(event)
	accounting := projectResponsesWSProviderDownstreamAccountingEvent(event)
	payloadPolicy := responsesWSProviderPayloadPolicyForEvent(upstreamEvent)
	if event.Err != nil {
		logCtx := context.Background()
		ctx := a.Context()
		if ctx != nil && ctx.Request != nil {
			logCtx = ctx.Request.Context()
		}
		frameKind := responsesws.FrameKind(0)
		if event.Frame != nil {
			frameKind = event.Frame.Kind()
		}
		logger.LogWarn(logCtx, fmt.Sprintf(
			"responses websocket provider downstream carried err: kind=%d origin=%d frame_kind=%d err=%s",
			event.Kind, payloadPolicy.PayloadOrigin, frameKind, event.Err.Error()))
	}
	payload := responsesWSProviderDownstreamPayload(event)
	if payloadPolicy.PayloadOrigin == responsesws.PayloadOriginProvider {
		responseID := responsesWSProviderDownstreamResponseID(event)
		attempt := a.turns.pending.attempt
		if attempt == nil {
			attempt = a.turns.active.attempt
		}
		switch a.checkProviderResponseID(attempt, responseID) {
		case responsesWSProviderResponseIDStaleFinalized:
			a.logIgnoredProviderEvent("provider_downstream_finalized_response_id", event.ChannelID, event.DetailOrigin, event.DetailPhase)
			return
		case responsesWSProviderResponseIDConflict:
			a.failClosed("responses_ws_provider_response_id_mismatch")
			return
		}
	}
	if event.Usage != nil && !payloadPolicy.CanCarryUsage {
		a.failClosed("responses_ws_provider_usage_without_provider_evidence")
		return
	}
	if a.turns.pending.attempt != nil {
		if responsesWSProviderDownstreamIsSyntheticBridgeCancelTerminal(event) {
			a.observeAndBufferPendingProviderEvent(event, accounting.UpstreamEvent)
			return
		}
	} else if observe && a.turns.active.attempt != nil {
		a.updateActiveProviderEvidence(accounting.UpstreamEvent)
	}
	hasProviderEvidence := accounting.HasProviderActivityEvidence
	if responsesWSProviderDownstreamIsSyntheticBridgeCancelTerminal(event) {
		if a.turns.active.attempt != nil {
			a.turns.active.bridgeCancelPendingAttemptID = a.turns.active.attempt.AttemptID
		}
		a.writeProxyLocal(payload)
		return
	}
	if payloadPolicy.PayloadOrigin != responsesws.PayloadOriginProvider {
		if len(payload) > 0 && !hasProviderEvidence {
			a.failClosed("responses_ws_unknown_provider_event_origin")
			return
		}
		if a.turns.pending.attempt != nil {
			if !a.appendPendingProviderLifecycle(accounting.UpstreamEvent) {
				return
			}
		}
		a.writeProxyLocal(payload)
		return
	}
	if !hasProviderEvidence {
		a.failClosed("responses_ws_unknown_provider_event_origin")
		return
	}
	receivedAt := event.ReceivedAt
	if receivedAt.IsZero() {
		receivedAt = time.Now()
	}
	if a.turns.pending.attempt != nil {
		if event.Usage != nil {
			mergeResponsesWSUsageEvent(a.turns.pending.attempt.Usage, event.Usage)
			// Pending downstream frames are replayed after the send result. Usage
			// attached to the frame has already entered the pending attempt, so the
			// replayed copy must keep the provider payload but not bill the same
			// delta a second time.
			event.Usage = nil
		}
		if event.Kind == ProviderDownstreamFrame && len(payload) > 0 {
			a.turns.pending.attempt.MarkFirstProviderResponse(receivedAt)
		}
		a.observeAndBufferPendingProviderEvent(event, accounting.UpstreamEvent)
		return
	}
	if event.Kind == ProviderDownstreamClose {
		if a.turns.active.attempt != nil {
			a.turns.active.attempt.MarkCompleted(receivedAt)
		}
		if err := a.finalizeActiveAttempt(); err != nil {
			a.handleActiveSettlementFailure(err)
			return
		}
		a.clearActiveTurn()
		a.markDownstreamCloseSent()
		if err := a.io.bridge.WriteClientFrame(responsesWSCloseMessageType, responsesWSProviderClosePayload(event.CloseCode, event.CloseReason), ResponsesWSWriteProvider); err != nil {
			a.close("client_write_failed")
			return
		}
		a.close("provider_closed")
		return
	}
	if event.Kind == ProviderDownstreamFrame && event.Frame == nil {
		return
	}
	if a.turns.active.attempt == nil {
		a.failClosed("responses_ws_provider_event_without_turn")
		return
	}
	if event.Kind == ProviderDownstreamFrame && len(payload) > 0 {
		a.turns.active.attempt.MarkFirstProviderResponse(receivedAt)
	}
	if event.Frame != nil && event.Frame.Kind() == responsesws.FrameKindBinary {
		if event.Usage != nil {
			mergeResponsesWSUsageEvent(a.turns.active.attempt.Usage, event.Usage)
		}
		if err := a.io.bridge.WriteClientTypedFrame(responsesws.NewBinaryFrame(payload), ResponsesWSWriteProvider); err != nil {
			a.close("client_write_failed")
		}
		return
	}

	classified := responsesws.ClassifyResponsesWSEvent(payload)
	if classified.Malformed {
		a.turns.active.attempt.MarkCompleted(receivedAt)
		a.handleMalformedProviderFrame(classified)
		return
	}
	if len(classified.NormalizedPayload) > 0 {
		payload = classified.NormalizedPayload
	}
	if classified.Response != nil {
		mergeResponsesWSTerminalResponse(a.turns.active.attempt.Usage, classified.Response)
	}
	if responsesWSShouldMergeAttachedFrameUsage(classified, event.Usage) {
		mergeResponsesWSUsageEvent(a.turns.active.attempt.Usage, event.Usage)
	}
	isTerminal := payloadPolicy.CanCarryTerminal && (classified.Kind == responsesws.ResponsesSuccessTerminal ||
		classified.Kind == responsesws.ResponsesFailedTerminal ||
		classified.Kind == responsesws.ResponsesCancelledTerminal)
	if isTerminal {
		a.logProviderTerminal(classified, receivedAt)
		a.turns.active.attempt.MarkCompleted(receivedAt)
		a.turns.active.attempt.MarkProviderTerminalEvidence(classified)
		if err := a.finalizeActiveAttempt(); err != nil {
			a.handleActiveSettlementFailure(err)
			return
		}
		a.processProviderPayloadAPIError(payload, event.ChannelID, "responses_ws_provider_frame")
		a.applyActiveTerminalSideEffects(classified)
	}
	if err := a.io.bridge.WriteClientTypedFrame(responsesws.NewTextFrame(payload), ResponsesWSWriteProvider); err != nil {
		a.close("client_write_failed")
		return
	}
	if !isTerminal {
		a.processProviderPayloadAPIError(payload, event.ChannelID, "responses_ws_provider_frame")
	}
}

func (a *ResponsesWSSessionActor) handleProviderUsageObserved(event ResponsesWSEventProviderUsageObserved) {
	if event.UpstreamSessionGeneration == "" && a.upstream.sessionGeneration != "" {
		a.logIgnoredProviderEvent("provider_usage_missing_generation", event.ChannelID, event.DetailOrigin, event.DetailPhase)
		return
	}
	if event.UpstreamSessionGeneration != "" && event.UpstreamSessionGeneration != a.upstream.sessionGeneration {
		return
	}
	if event.ChannelID > 0 && event.ChannelID != a.upstream.channelID {
		a.failClosed("responses_ws_provider_channel_mismatch")
		return
	}
	if !a.providerEventAttemptMatches(event.AttemptID) {
		a.logIgnoredProviderEvent("provider_usage_attempt_mismatch", event.ChannelID, event.DetailOrigin, event.DetailPhase)
		return
	}
	if event.Usage == nil {
		return
	}
	accounting := projectResponsesWSProviderUsageAccountingEvent(event)
	if !responsesWSProviderPayloadPolicyForEvent(accounting.UpstreamEvent).CanCarryUsage {
		a.failClosed("responses_ws_provider_usage_without_provider_evidence")
		return
	}
	responseID := responsesWSProviderUsageResponseID(event)
	attempt := a.turns.pending.attempt
	if attempt == nil {
		attempt = a.turns.active.attempt
	}
	switch a.checkProviderResponseID(attempt, responseID) {
	case responsesWSProviderResponseIDStaleFinalized:
		a.logIgnoredProviderEvent("provider_usage_finalized_response_id", event.ChannelID, event.DetailOrigin, event.DetailPhase)
		return
	case responsesWSProviderResponseIDConflict:
		a.failClosed("responses_ws_provider_response_id_mismatch")
		return
	}
	dropUnpricedUsage := a.shouldDropUnpricedProviderUsage(event.Usage)
	if a.turns.pending.attempt != nil {
		if !a.appendPendingProviderLifecycle(accounting.UpstreamEvent) {
			return
		}
		if !dropUnpricedUsage {
			mergeResponsesWSUsageEvent(a.turns.pending.attempt.Usage, event.Usage)
		}
		return
	}
	if a.turns.active.attempt != nil {
		a.updateActiveProviderEvidence(accounting.UpstreamEvent)
		if !dropUnpricedUsage {
			mergeResponsesWSUsageEvent(a.turns.active.attempt.Usage, event.Usage)
		}
	}
}

func (a *ResponsesWSSessionActor) shouldDropUnpricedProviderUsage(usage *types.UsageEvent) bool {
	if usage == nil || usage.Source != types.UsageSourceInputAudioTranscription {
		return false
	}
	modelName := a.billingModelName()
	if transcriptionPricingConfigured(modelName) {
		return false
	}
	recordUsageObservedUnbilled(string(types.UsageSourceInputAudioTranscription), modelName)
	return true
}

func (a *ResponsesWSSessionActor) billingModelName() string {
	if a == nil {
		return ""
	}
	if a.turns.pending.attempt != nil && a.turns.pending.attempt.Quota != nil {
		return strings.TrimSpace(a.turns.pending.attempt.Quota.ModelName())
	}
	if a.turns.active.attempt != nil && a.turns.active.attempt.Quota != nil {
		return strings.TrimSpace(a.turns.active.attempt.Quota.ModelName())
	}
	_, billingModel := responsesWSCurrentModelNames(a.Context())
	return strings.TrimSpace(billingModel)
}

func transcriptionPricingConfigured(modelName string) bool {
	if model.PricingInstance == nil {
		return false
	}
	price := model.PricingInstance.GetPrice(strings.TrimSpace(modelName))
	if price == nil || price.ExtraRatios == nil {
		return false
	}
	ratios := price.ExtraRatios.Data()
	_, ok := ratios[config.UsageExtraInputAudioTranscription]
	return ok
}

func (a *ResponsesWSSessionActor) handleProviderBusinessError(event ResponsesWSEventProviderBusinessError) {
	if event.UpstreamSessionGeneration == "" && a.upstream.sessionGeneration != "" {
		a.logIgnoredProviderEvent("provider_business_error_missing_generation", event.ChannelID, event.DetailOrigin, event.DetailPhase)
		return
	}
	if event.UpstreamSessionGeneration != "" && event.UpstreamSessionGeneration != a.upstream.sessionGeneration {
		return
	}
	if event.ChannelID > 0 && event.ChannelID != a.upstream.channelID {
		a.failClosed("responses_ws_provider_channel_mismatch")
		return
	}
	if !a.providerEventAttemptMatches(event.AttemptID) {
		a.logIgnoredProviderEvent("provider_business_error_attempt_mismatch", event.ChannelID, event.DetailOrigin, event.DetailPhase)
		return
	}
	upstreamEvent := upstreamEventFromProviderBusinessError(event)
	if a.turns.pending.attempt != nil {
		if !a.appendPendingProviderLifecycle(upstreamEvent) {
			return
		}
	}
	if a.turns.active.attempt != nil {
		a.updateActiveProviderEvidence(upstreamEvent)
	}
	if event.Err != nil {
		a.writeProxyLocal(responsesWSErrorFromErr(event.Err))
	}
	a.close("provider_business_error")
}

func (a *ResponsesWSSessionActor) handleProviderRecvFailed(event ResponsesWSEventProviderRecvFailed) {
	a.handleProviderRecvFailedWithObservation(event, true)
}

func (a *ResponsesWSSessionActor) handleProviderRecvFailedReplayed(event ResponsesWSEventProviderRecvFailed) {
	a.handleProviderRecvFailedWithObservation(event, false)
}

func (a *ResponsesWSSessionActor) handleProviderRecvFailedWithObservation(event ResponsesWSEventProviderRecvFailed, observe bool) {
	if event.UpstreamSessionGeneration == "" && a.upstream.sessionGeneration != "" {
		a.logIgnoredProviderEvent("provider_recv_failed_missing_generation", event.ChannelID, event.DetailOrigin, event.DetailPhase)
		return
	}
	if event.UpstreamSessionGeneration != "" && event.UpstreamSessionGeneration != a.upstream.sessionGeneration {
		return
	}
	if event.ChannelID > 0 && event.ChannelID != a.upstream.channelID {
		a.failClosed("responses_ws_provider_channel_mismatch")
		return
	}
	if !a.providerRecvFailureAttemptMatches(event) {
		a.logIgnoredProviderEvent("provider_recv_failed_attempt_mismatch", event.ChannelID, event.DetailOrigin, event.DetailPhase)
		return
	}
	upstreamEvent := upstreamEventFromProviderRecvFailed(event)
	if a.turns.pending.attempt != nil {
		a.observeAndBufferPendingProviderFailure(event, upstreamEvent)
		return
	}
	if a.turns.active.attempt != nil {
		if observe {
			a.updateActiveProviderEvidence(upstreamEvent)
		}
		if a.completeBridgeCancelOnStreamEOF(event) {
			return
		}
		if responsesWSProviderLifecyclePolicyForEvent(upstreamEvent).ProviderMalformedClientPayload {
			a.handleProviderMalformedRecvFailed(event)
			return
		}
	} else {
		if responsesWSIdleRecvFailureClosesSession(upstreamEvent) {
			if payload := responsesWSProviderRecvFailureClientPayload(event); len(payload) > 0 {
				a.writeProxyLocal(payload)
			}
			a.close("provider_recv_failed")
			return
		}
		a.logIgnoredProviderEvent("provider_recv_failed_without_turn", event.ChannelID, event.DetailOrigin, event.DetailPhase)
		return
	}
	if payload := responsesWSProviderRecvFailureClientPayload(event); len(payload) > 0 {
		a.writeProxyLocal(payload)
	}
	a.close("provider_recv_failed")
}

func (a *ResponsesWSSessionActor) completeBridgeCancelOnStreamEOF(event ResponsesWSEventProviderRecvFailed) bool {
	if a == nil || a.turns.active.attempt == nil || !responsesWSProviderLifecyclePolicyForEvent(upstreamEventFromProviderRecvFailed(event)).BridgeStreamEOF {
		return false
	}
	pendingAttemptID := strings.TrimSpace(a.turns.active.bridgeCancelPendingAttemptID)
	if pendingAttemptID == "" {
		return false
	}
	if event.AttemptID != "" && event.AttemptID != pendingAttemptID {
		return false
	}
	if a.turns.active.attempt.AttemptID != pendingAttemptID {
		return false
	}
	receivedAt := event.ReceivedAt
	if receivedAt.IsZero() {
		receivedAt = time.Now()
	}
	a.turns.active.attempt.MarkCompleted(receivedAt)
	if err := a.finalizeActiveAttempt(); err != nil {
		a.handleActiveSettlementFailure(err)
		return true
	}
	a.clearActiveTurn()
	return true
}

func (a *ResponsesWSSessionActor) handleProviderMalformedRecvFailed(event ResponsesWSEventProviderRecvFailed) {
	receivedAt := event.ReceivedAt
	if receivedAt.IsZero() {
		receivedAt = time.Now()
	}
	if a.turns.active.attempt != nil {
		a.turns.active.attempt.MarkCompleted(receivedAt)
	}
	payload := responsesWSProviderRecvFailureClientPayload(event)
	if err := a.finalizeActiveAttempt(); err != nil {
		a.handleActiveSettlementFailure(err)
		return
	}
	a.clearActiveTurn()
	a.writeProxyLocal(payload)
	a.close("responses_ws_provider_protocol_error")
}

func (a *ResponsesWSSessionActor) handleProviderClosed(event ResponsesWSEventProviderClosed) {
	if event.UpstreamSessionGeneration == "" && a.upstream.sessionGeneration != "" {
		a.logIgnoredProviderEvent("provider_closed_missing_generation", event.ChannelID, event.DetailOrigin, event.DetailPhase)
		return
	}
	if event.UpstreamSessionGeneration != "" && event.UpstreamSessionGeneration != a.upstream.sessionGeneration {
		return
	}
	if event.ChannelID > 0 && event.ChannelID != a.upstream.channelID {
		a.failClosed("responses_ws_provider_channel_mismatch")
		return
	}
	if !a.providerSessionLifecycleAttemptMatches(event.AttemptID) {
		a.logIgnoredProviderEvent("provider_closed_attempt_mismatch", event.ChannelID, event.DetailOrigin, event.DetailPhase)
		return
	}
	receivedAt := event.ReceivedAt
	if receivedAt.IsZero() {
		receivedAt = time.Now()
	}
	upstreamEvent := upstreamEventFromProviderClosed(event)
	if a.turns.pending.attempt != nil {
		a.observeAndBufferPendingProviderEvent(ResponsesWSEventProviderDownstream{
			UpstreamSessionGeneration: event.UpstreamSessionGeneration,
			ChannelID:                 event.ChannelID,
			AttemptID:                 event.AttemptID,
			Kind:                      ProviderDownstreamClose,
			CloseCode:                 event.Code,
			CloseReason:               event.Reason,
			Err:                       event.Err,
			DetailOrigin:              event.DetailOrigin,
			DetailPhase:               event.DetailPhase,
			ReceivedAt:                receivedAt,
		}, upstreamEvent)
		return
	}
	if a.turns.active.attempt != nil {
		a.updateActiveProviderEvidence(upstreamEvent)
		a.turns.active.attempt.MarkCompleted(receivedAt)
	}
	if err := a.finalizeActiveAttempt(); err != nil {
		a.handleActiveSettlementFailure(err)
		return
	}
	a.clearActiveTurn()
	a.markDownstreamCloseSent()
	if a.closeProviderDownstream(event) {
		a.close("provider_closed")
		return
	}
	if a.io.bridge != nil {
		if err := a.io.bridge.WriteClientFrame(responsesWSCloseMessageType, responsesWSProviderClosePayload(event.Code, event.Reason), ResponsesWSWriteProvider); err != nil {
			a.close("client_write_failed")
			return
		}
	}
	a.close("provider_closed")
}

func (a *ResponsesWSSessionActor) closeProviderDownstream(event ResponsesWSEventProviderClosed) bool {
	if a == nil || a.io.client == nil {
		return false
	}
	a.io.client.Close(wsconn.CloseInfo{
		Kind:   wsconn.CloseKindGracefulShutdown,
		Code:   wsconn.SanitizeWireCloseCode(event.Code),
		Reason: event.Reason,
		Err:    event.Err,
	})
	return true
}

func (a *ResponsesWSSessionActor) processProviderPayloadAPIError(payload []byte, channelID int, source string) {
	if a == nil || len(payload) == 0 {
		return
	}
	apiErr := runtimesession.ProviderAPIErrorFromPayload(payload)
	if apiErr == nil {
		return
	}
	if !a.markProviderAPIErrorSeen(apiErr, source) {
		return
	}
	channel := a.providerPayloadChannel(channelID)
	processProviderAPIError(a.Context(), channel, apiErr, source)
}

func (a *ResponsesWSSessionActor) markProviderAPIErrorSeen(apiErr *types.OpenAIErrorWithStatusCode, source string) bool {
	if a == nil || apiErr == nil {
		return true
	}
	attempt := a.turns.active.attempt
	if attempt == nil {
		attempt = a.turns.pending.attempt
	}
	if attempt == nil {
		return true
	}
	// Trade-off: dedupe only within the current turn attempt. This suppresses
	// repeated provider frames for the same failure without hiding the same
	// provider-side error if a later user turn fails independently.
	key := providerAPIErrorDedupeKey(apiErr, source)
	if _, ok := attempt.providerAPIErrorKeys[key]; ok {
		return false
	}
	if attempt.providerAPIErrorKeys == nil {
		attempt.providerAPIErrorKeys = make(map[string]struct{}, 1)
	}
	attempt.providerAPIErrorKeys[key] = struct{}{}
	return true
}

func providerAPIErrorDedupeKey(apiErr *types.OpenAIErrorWithStatusCode, source string) string {
	if apiErr == nil {
		return ""
	}
	return fmt.Sprintf(
		"%s|%d|%s|%v|%s|%s",
		strings.TrimSpace(source),
		apiErr.StatusCode,
		strings.TrimSpace(apiErr.Type),
		apiErr.Code,
		strings.TrimSpace(apiErr.Message),
		strings.TrimSpace(apiErr.Param),
	)
}

func (a *ResponsesWSSessionActor) providerPayloadChannel(channelID int) *model.Channel {
	if a == nil {
		return nil
	}
	ctx := a.Context()
	if ctx != nil {
		if raw, ok := ctx.Get("responses_ws_selected_channel"); ok {
			if channel, ok := raw.(*model.Channel); ok && channel != nil {
				return channel
			}
		}
		if raw, ok := ctx.Get("responses_ws_selected_channel_snapshot"); ok {
			if snapshot, ok := raw.(*SelectedChannelSnapshot); ok && snapshot != nil && snapshot.Channel != nil {
				return snapshot.Channel
			}
		}
	}
	if channelID <= 0 {
		channelID = a.upstream.channelID
	}
	if channelID <= 0 {
		return nil
	}
	channel, err := fetchChannelById(channelID)
	if err != nil {
		return nil
	}
	return channel
}

func (a *ResponsesWSSessionActor) handleMalformedProviderFrame(classified responsesws.ResponsesTerminalResult) {
	if a == nil || a.turns.active.attempt == nil {
		return
	}
	payload := responsesWSProviderProtocolErrorPayload(classified.MalformedError)
	if err := a.finalizeActiveAttempt(); err != nil {
		a.handleActiveSettlementFailure(err)
		return
	}
	a.clearActiveTurn()
	if err := a.io.bridge.WriteClientFrame(responsesWSTextMessageType, payload, ResponsesWSWriteProvider); err != nil {
		a.close("client_write_failed")
		return
	}
	a.close("responses_ws_provider_protocol_error")
}

func responsesWSProviderRecvFailureClientPayload(event ResponsesWSEventProviderRecvFailed) []byte {
	if payload := responsesws.ClientPayloadFromError(event.Err); len(payload) > 0 {
		return payload
	}
	if responsesWSProviderLifecyclePolicyForEvent(upstreamEventFromProviderRecvFailed(event)).ProviderMalformedClientPayload {
		return responsesWSProviderProtocolErrorPayload("")
	}
	if errors.Is(event.Err, responsesws.ErrInvalidBridgeStreamPayload) {
		return responsesWSProviderProtocolErrorPayload("malformed responses websocket bridge stream payload")
	}
	if errors.Is(event.Err, requester.ErrStreamLineTooLarge) {
		return responsesWSProviderProtocolErrorPayload(event.Err.Error())
	}
	return nil
}

func responsesWSProviderProtocolErrorPayload(message string) []byte {
	message = strings.TrimSpace(message)
	if message == "" {
		message = "provider returned malformed responses websocket frame"
	}
	return responsesWSErrorPayload(http.StatusBadGateway, "responses_ws_provider_protocol_error", message)
}

func (a *ResponsesWSSessionActor) bufferPendingProviderEvent(event ResponsesWSEventProviderDownstream, upstream responsesws.UpstreamEvent) bool {
	if a == nil {
		return false
	}
	buffered, overLimit := a.turns.pending.provider.journal.AppendDownstream(event, upstream, config.ResponsesWSPendingProviderEventsMaxBytes())
	if overLimit {
		a.failClosed("responses_ws_pending_provider_buffer_full")
		return false
	}
	return buffered
}

func (a *ResponsesWSSessionActor) observeAndBufferPendingProviderEvent(event ResponsesWSEventProviderDownstream, upstream responsesws.UpstreamEvent) bool {
	if a == nil || a.turns.pending.attempt == nil {
		return false
	}
	return a.bufferPendingProviderEvent(event, upstream)
}

func (a *ResponsesWSSessionActor) observeAndBufferPendingProviderFailure(event ResponsesWSEventProviderRecvFailed, upstream responsesws.UpstreamEvent) {
	if a == nil || a.turns.pending.attempt == nil {
		return
	}
	if a.turns.pending.provider.journal.AppendFailure(event, upstream) {
		a.failClosed("responses_ws_pending_provider_buffer_full")
	}
}

func (a *ResponsesWSSessionActor) applyActiveTerminalSideEffects(classified responsesws.ResponsesTerminalResult) {
	if a == nil {
		return
	}
	if classified.Response != nil {
		a.rememberFinalizedResponseID(classified.Response.ID)
	}
	switch classified.Kind {
	case responsesws.ResponsesSuccessTerminal:
		RecordResponsesTurnSuccess(a.Context(), a.turns.active.affinity, classified.Response)
		a.turns.history.lastFinal = classified.Response
		a.clearActiveTurn()
	case responsesws.ResponsesFailedTerminal:
		if classified.ContinuationMiss {
			attemptedPreviousResponseID := ""
			if a.turns.active.attempt != nil {
				attemptedPreviousResponseID = a.turns.active.attempt.AttemptedPreviousResponseID
			}
			a.applyContinuationMissSideEffects(a.turns.active.affinity, a.turns.active.channelID, attemptedPreviousResponseID)
		}
		a.clearActiveTurn()
	case responsesws.ResponsesCancelledTerminal:
		a.clearActiveTurn()
	}
}

func (a *ResponsesWSSessionActor) applyContinuationMissSideEffects(turn *ResponsesTurnAffinity, ownerChannelID int, attemptedPreviousResponseID string) {
	if a == nil {
		return
	}
	attemptedPreviousResponseID = strings.TrimSpace(attemptedPreviousResponseID)
	if attemptedPreviousResponseID == "" && turn != nil {
		attemptedPreviousResponseID = strings.TrimSpace(turn.PreviousResponseID)
	}
	ClearResponsesTurnContinuationMissBindings(turn, ownerChannelID, attemptedPreviousResponseID)
	if attemptedPreviousResponseID == "" || a.turns.history.lastFinal == nil {
		return
	}
	if strings.TrimSpace(a.turns.history.lastFinal.ID) == attemptedPreviousResponseID {
		a.turns.history.lastFinal = nil
	}
}

func (a *ResponsesWSSessionActor) finalizeActiveAttempt() error {
	if a == nil || a.turns.active.attempt == nil {
		return nil
	}
	input := a.buildActiveSettlementInput("active_finalize", ResponsesWSZeroChargeProof{})
	if _, _, err := a.applyActiveSettlement(input); err != nil {
		a.logErrorf("responses websocket active settlement failed: %v", err)
		return err
	}
	return nil
}

func (a *ResponsesWSSessionActor) handleActiveSettlementFailure(err error) {
	if a == nil {
		return
	}
	if err != nil {
		a.logErrorf("responses websocket active settlement failed before side effects: %v", err)
	}
	a.writeProxyLocal(responsesWSErrorPayload(http.StatusInternalServerError, "quota_settlement_failed", responsesWSStaticErrorMessage("quota_settlement_failed")))
	a.close("quota_settlement_failed")
}

func (a *ResponsesWSSessionActor) clearPendingTurn(reason string) responsesWSPendingCleanup {
	if a == nil {
		return responsesWSPendingCleanup{}
	}
	cleanup := a.turns.ClearPending()
	if cleanup.attempt != nil || cleanup.openingID != "" || cleanup.phase != responsesWSPendingTurnNone ||
		len(cleanup.provider.journal.entries) > 0 ||
		cleanup.provider.bridgeOpenLocalErrorAttemptID != "" ||
		cleanup.provider.bridgeOpenProviderErr != nil ||
		cleanup.cancel.sendAttemptID != "" ||
		cleanup.cancel.createAttemptID != "" {
		attemptID := ""
		if cleanup.attempt != nil {
			attemptID = cleanup.attempt.AttemptID
		}
		a.logDebugf(
			"responses websocket pending turn cleared: reason=%s attempt_id=%s opening_id=%s phase=%d provider_journal_entries=%d provider_evidence=%t send_cancel=%t create_cancel=%t",
			responsesWSSafeDiagnosticValue(strings.TrimSpace(reason)),
			responsesWSSafeDiagnosticValue(attemptID),
			responsesWSSafeDiagnosticValue(cleanup.openingID),
			cleanup.phase,
			len(cleanup.provider.journal.entries),
			cleanup.provider.journal.Project().HasActivity(),
			cleanup.cancel.sendAttemptID != "",
			cleanup.cancel.createAttemptID != "",
		)
	}
	return cleanup
}

func (a *ResponsesWSSessionActor) finishActiveTurn(reason string, attemptID string) error {
	if a == nil {
		return nil
	}
	activeAttemptID := ""
	if a.turns.active.attempt != nil {
		activeAttemptID = a.turns.active.attempt.AttemptID
		a.clearPendingSendCancel(a.turns.active.attempt.AttemptID)
		a.clearPendingCreateCancel(a.turns.active.attempt.AttemptID)
	}
	a.stopActiveTurnWatchdog()
	err := a.turns.FinishActive(responsesWSTurnFinalization{attemptID: strings.TrimSpace(attemptID)})
	if err == nil && activeAttemptID != "" {
		a.logDebugf(
			"responses websocket active turn finished: reason=%s attempt_id=%s",
			responsesWSSafeDiagnosticValue(strings.TrimSpace(reason)),
			responsesWSSafeDiagnosticValue(activeAttemptID),
		)
	}
	return err
}

func (a *ResponsesWSSessionActor) clearActiveTurn() {
	if err := a.finishActiveTurn("clear_active_turn", ""); err != nil {
		a.logErrorf("responses websocket active finish transition failed: %v", err)
	}
	a.turns.pending.phase = responsesWSPendingTurnNone
	a.state = responsesWSStateIdle
}

func (a *ResponsesWSSessionActor) handleClientClosed(err error) {
	if err != nil && !isResponsesWSExpectedClientDisconnectError(err) {
		a.logDebugf("responses websocket client close event: %T: %v", err, err)
	}
	a.close("client_closed")
}

func (a *ResponsesWSSessionActor) logProviderTerminal(classified responsesws.ResponsesTerminalResult, receivedAt time.Time) {
	if a == nil {
		return
	}
	elapsedMs := int64(-1)
	if !a.turns.opening.startedAt.IsZero() {
		elapsedMs = receivedAt.Sub(a.turns.opening.startedAt).Milliseconds()
	}
	promptTokens := 0
	completionTokens := 0
	totalTokens := 0
	status := ""
	if classified.Response != nil && classified.Response.Usage != nil {
		promptTokens = classified.Response.Usage.InputTokens
		completionTokens = classified.Response.Usage.OutputTokens
		totalTokens = classified.Response.Usage.TotalTokens
	}
	if classified.Response != nil {
		status = classified.Response.Status
	}
	logger.LogDebug(a.logContext(), fmt.Sprintf(
		"responses websocket provider terminal: event_type=%s kind=%d status=%s continuation_miss=%t elapsed_ms=%d channel_id=%d prompt_tokens=%d completion_tokens=%d total_tokens=%d",
		classified.EventType,
		classified.Kind,
		status,
		classified.ContinuationMiss,
		elapsedMs,
		a.turns.active.channelID,
		promptTokens,
		completionTokens,
		totalTokens,
	))
}

func (a *ResponsesWSSessionActor) logClose(reason string) {
	if a == nil {
		return
	}
	elapsedMs := int64(-1)
	if !a.turns.opening.startedAt.IsZero() {
		elapsedMs = time.Since(a.turns.opening.startedAt).Milliseconds()
	}
	lastProviderActivityOrigin := responsesws.RecvDetailOrigin("")
	if origin := a.turns.pending.provider.journal.Project().LastActivityOrigin(); origin != "" {
		lastProviderActivityOrigin = origin
	} else if origin := a.turns.active.evidence.LastActivityOrigin(); origin != "" {
		lastProviderActivityOrigin = origin
	}
	logger.LogDebug(a.logContext(), fmt.Sprintf(
		"responses websocket session closing: reason=%s state=%d pending_phase=%d pending_attempt=%t active_attempt=%t pending_provider_events=%d pending_provider_evidence=%t last_provider_activity_origin=%s active_channel_id=%d session_channel_id=%d downstream_close_sent=%t client_closed=%t elapsed_ms=%d",
		strings.TrimSpace(reason),
		a.state,
		a.turns.pending.phase,
		a.turns.pending.attempt != nil,
		a.turns.active.attempt != nil,
		len(a.turns.pending.provider.journal.Replay()),
		a.hasPendingProviderEvidence(),
		lastProviderActivityOrigin,
		a.turns.active.channelID,
		a.upstream.channelID,
		a.closing.downstreamCloseSent.Load(),
		a.closing.clientClosed.Load(),
		elapsedMs,
	))
}

func (a *ResponsesWSSessionActor) logContext() context.Context {
	if a == nil {
		return context.Background()
	}
	ctx := a.Context()
	if ctx != nil && ctx.Request != nil {
		return ctx.Request.Context()
	}
	return context.Background()
}

func (a *ResponsesWSSessionActor) logErrorf(format string, args ...any) {
	logger.LogError(a.logContext(), fmt.Sprintf(format, args...))
}

func (a *ResponsesWSSessionActor) logWarnf(format string, args ...any) {
	logger.LogWarn(a.logContext(), fmt.Sprintf(format, args...))
}

func (a *ResponsesWSSessionActor) logDebugf(format string, args ...any) {
	logger.LogDebug(a.logContext(), fmt.Sprintf(format, args...))
}

func (a *ResponsesWSSessionActor) observeSettlementConflict(kind string, decision ResponsesWSSettlementDecision) {
	if a == nil {
		return
	}
	kind = strings.TrimSpace(kind)
	if kind == "" {
		kind = string(ResponsesWSSettlementFlagContradictoryInput)
	}
	a.logWarnf(
		"responses websocket settlement conflict: kind=%s decision=%s flags=%v",
		kind,
		responsesWSSafeDiagnosticValue(decision.DecisionKey),
		decision.Flags,
	)
	recordResponsesWSSettlementConflict(kind)
}

func (a *ResponsesWSSessionActor) isBusy() bool {
	return a.turns.pending.phase != responsesWSPendingTurnNone || a.turns.pending.attempt != nil || a.turns.active.attempt != nil || a.state == responsesWSStateOpening || a.state == responsesWSStatePendingPrepare || a.state == responsesWSStatePendingSend || a.state == responsesWSStateInFlight
}

func (a *ResponsesWSSessionActor) writeProxyLocal(payload []byte) {
	if a == nil || a.io.bridge == nil || len(payload) == 0 {
		return
	}
	if err := a.io.bridge.WriteClientFrame(responsesWSTextMessageType, payload, ResponsesWSWriteProxyLocal); err != nil {
		a.markClientClosed(err)
		a.requestCloseIntent("client_write_failed")
	}
}

func (a *ResponsesWSSessionActor) requestCloseIntent(reason string) {
	if a == nil || a.closing.closed.Load() {
		return
	}
	if !a.closing.closeIntentPosted.CompareAndSwap(false, true) {
		return
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "close_intent"
	}
	a.postInternalEvent(ResponsesWSEventCloseIntent{Reason: reason}, "close_intent_post")
}

func (a *ResponsesWSSessionActor) postInternalEvent(event ResponsesWSEvent, label string) bool {
	if a == nil || a.closing.closed.Load() {
		return false
	}
	select {
	case a.events <- event:
		return true
	case <-a.done:
		return false
	default:
		timeout := a.reliablePostTimeoutValue()
		logLabel := strings.TrimSpace(label)
		go func() {
			defer recoverResponsesWSGoroutine(logLabel, nil)
			if ok, timedOut := a.postEventBounded(event, timeout); !ok && timedOut {
				eventType := responsesWSEventTypeLabel(event)
				a.logErrorf("responses websocket internal event post timed out: label=%s event_type=%s timeout=%s", logLabel, eventType, timeout)
				recordResponsesWSEventPostTimeout(eventType)
			}
		}()
		return false
	}
}

func (a *ResponsesWSSessionActor) clearPendingProviderState(reason string) {
	if a == nil {
		return
	}
	provider := a.turns.pending.provider
	hasState := len(provider.journal.entries) > 0 ||
		provider.bridgeOpenLocalErrorAttemptID != "" ||
		provider.bridgeOpenProviderErr != nil
	a.turns.ResetPendingProvider()
	if hasState {
		a.logDebugf(
			"responses websocket pending provider state reset: reason=%s provider_journal_entries=%d provider_evidence=%t bridge_local_error_pending=%t bridge_provider_error_pending=%t",
			responsesWSSafeDiagnosticValue(strings.TrimSpace(reason)),
			len(provider.journal.entries),
			provider.journal.Project().HasActivity(),
			provider.bridgeOpenLocalErrorAttemptID != "",
			provider.bridgeOpenProviderErr != nil,
		)
	}
}

func (a *ResponsesWSSessionActor) hasPendingProviderEvidence() bool {
	return a != nil && a.turns.pending.provider.journal.Project().HasActivity()
}

func (a *ResponsesWSSessionActor) providerEventAttemptMatches(attemptID string) bool {
	if a == nil || strings.TrimSpace(attemptID) == "" {
		return false
	}
	if a.turns.pending.attempt != nil {
		return a.turns.pending.attempt.AttemptID == attemptID
	}
	if a.turns.active.attempt != nil {
		return a.turns.active.attempt.AttemptID == attemptID
	}
	return true
}

func (a *ResponsesWSSessionActor) providerSessionLifecycleAttemptMatches(attemptID string) bool {
	if a == nil || strings.TrimSpace(attemptID) == "" {
		return true
	}
	if a.turns.pending.attempt != nil || a.turns.active.attempt != nil {
		return a.providerEventAttemptMatches(attemptID)
	}
	return true
}

func (a *ResponsesWSSessionActor) providerRecvFailureAttemptMatches(event ResponsesWSEventProviderRecvFailed) bool {
	if responsesWSIdleRecvFailureClosesSession(upstreamEventFromProviderRecvFailed(event)) {
		return a.providerSessionLifecycleAttemptMatches(event.AttemptID)
	}
	return a.providerEventAttemptMatches(event.AttemptID)
}

func responsesWSIdleRecvFailureClosesSession(event responsesws.UpstreamEvent) bool {
	return responsesWSProviderLifecyclePolicyForEvent(event).IdleRecvFailureClosesSession
}

func (a *ResponsesWSSessionActor) appendPendingProviderLifecycle(event responsesws.UpstreamEvent) bool {
	if a == nil || a.turns.pending.attempt == nil {
		return false
	}
	if a.turns.pending.provider.journal.AppendLifecycle(event) {
		a.failClosed("responses_ws_pending_provider_buffer_full")
		return false
	}
	return true
}

func (a *ResponsesWSSessionActor) updateActiveProviderEvidence(event responsesws.UpstreamEvent) {
	if a == nil || a.turns.active.attempt == nil {
		return
	}
	hasProviderEvidence := responsesws.UpstreamEventHasProviderEvidence(event)
	a.turns.active.evidence.Observe(responsesws.NewProviderObservation(event))
	if hasProviderEvidence {
		a.armActiveTurnWatchdog()
	}
}

func upstreamEventFromProviderDownstream(event ResponsesWSEventProviderDownstream) responsesws.UpstreamEvent {
	upstream := responsesws.UpstreamEvent{
		Frame:        responsesWSCloneFramePtr(event.Frame),
		Usage:        event.Usage,
		AttemptID:    event.AttemptID,
		ResponseID:   event.ResponseID,
		DetailOrigin: event.DetailOrigin,
		DetailPhase:  event.DetailPhase,
		Err:          event.Err,
	}
	if event.Kind == ProviderDownstreamClose {
		upstream.ProviderClose = &responsesws.ProviderClose{
			Code:   event.CloseCode,
			Reason: event.CloseReason,
			Err:    event.Err,
		}
	}
	return upstream
}

func upstreamEventFromProviderUsage(event ResponsesWSEventProviderUsageObserved) responsesws.UpstreamEvent {
	return responsesws.UpstreamEvent{
		Usage:        event.Usage,
		AttemptID:    event.AttemptID,
		ResponseID:   event.ResponseID,
		DetailOrigin: event.DetailOrigin,
		DetailPhase:  event.DetailPhase,
	}
}

func upstreamEventFromProviderClosed(event ResponsesWSEventProviderClosed) responsesws.UpstreamEvent {
	return responsesws.UpstreamEvent{
		ProviderClose: &responsesws.ProviderClose{
			Code:   event.Code,
			Reason: event.Reason,
			Err:    event.Err,
		},
		AttemptID:    event.AttemptID,
		DetailOrigin: event.DetailOrigin,
		DetailPhase:  event.DetailPhase,
		Err:          event.Err,
	}
}

func upstreamEventFromProviderBusinessError(event ResponsesWSEventProviderBusinessError) responsesws.UpstreamEvent {
	return responsesws.UpstreamEvent{
		AttemptID:    event.AttemptID,
		DetailOrigin: event.DetailOrigin,
		DetailPhase:  event.DetailPhase,
		Err:          event.Err,
	}
}

func upstreamEventFromProviderRecvFailed(event ResponsesWSEventProviderRecvFailed) responsesws.UpstreamEvent {
	return responsesws.UpstreamEvent{
		AttemptID:    event.AttemptID,
		DetailOrigin: event.DetailOrigin,
		DetailPhase:  event.DetailPhase,
		Err:          event.Err,
	}
}

func upstreamEventFromProxyLocalError(event ResponsesWSEventProxyLocalError) responsesws.UpstreamEvent {
	origin := event.DetailOrigin
	var err error
	if event.ProviderAPIError != nil {
		err = event.ProviderAPIError
	}
	return responsesws.UpstreamEvent{
		AttemptID:    event.AttemptID,
		DetailOrigin: origin,
		DetailPhase:  event.DetailPhase,
		Err:          err,
	}
}

func upstreamEventFromBridgeOpenProviderError(event ResponsesWSEventBridgeOpenProviderError) responsesws.UpstreamEvent {
	var err error
	if event.ProviderAPIError != nil {
		err = event.ProviderAPIError
	}
	return responsesws.UpstreamEvent{
		AttemptID:    event.AttemptID,
		DetailOrigin: responsesws.RecvDetailOriginBridgeOpenProviderError,
		DetailPhase:  event.DetailPhase,
		Err:          err,
	}
}

func upstreamEventFromBridgeOpenLocalError(event ResponsesWSEventBridgeOpenLocalError) responsesws.UpstreamEvent {
	return responsesws.UpstreamEvent{
		AttemptID:    event.AttemptID,
		DetailOrigin: responsesws.RecvDetailOriginBridgeStreamError,
		DetailPhase:  event.DetailPhase,
		Err:          responsesws.NewClientPayloadError(responsesws.ErrBridgeOpenCancelled, event.Payload),
	}
}

type responsesWSPendingBufferedTerminal struct {
	event                       ResponsesWSEventProviderDownstream
	payload                     []byte
	classified                  responsesws.ResponsesTerminalResult
	candidate                   *ResponsesTurnAffinity
	ownerChannelID              int
	attemptedPreviousResponseID string
}

func (a *ResponsesWSSessionActor) applyBufferedPendingTerminalEvidence() (*responsesWSPendingBufferedTerminal, bool) {
	if a == nil || a.turns.pending.attempt == nil ||
		responsesWSTransportSendStatus(a.turns.pending.attempt.TransportResult) == responsesws.ResponsesWSTransportSendNotAttempted ||
		len(a.turns.pending.provider.journal.Replay()) == 0 {
		return nil, false
	}
	for _, entry := range a.turns.pending.provider.journal.Replay() {
		if entry.Downstream == nil {
			continue
		}
		event := *entry.Downstream
		payload := responsesWSProviderDownstreamPayload(event)
		payloadPolicy := responsesWSProviderPayloadPolicyForEvent(upstreamEventFromProviderDownstream(event))
		if payloadPolicy.PayloadOrigin != responsesws.PayloadOriginProvider || !payloadPolicy.CanCarryTerminal || len(payload) == 0 {
			continue
		}
		if event.Frame != nil && event.Frame.Kind() == responsesws.FrameKindBinary {
			continue
		}
		classified := responsesws.ClassifyResponsesWSEvent(payload)
		if classified.Malformed {
			continue
		}
		// Trade-off: close replay may merge usage/terminal evidence before settlement
		// so the pure core sees the strongest accounting input. User-visible and
		// control-plane side effects still wait for settlement success; ordinary
		// contradictory evidence is recorded as diagnostics, not as a side-effect
		// blocker.
		if classified.Response != nil {
			mergeResponsesWSTerminalResponse(a.turns.pending.attempt.Usage, classified.Response)
		}
		switch classified.Kind {
		case responsesws.ResponsesSuccessTerminal:
			a.turns.pending.attempt.MarkCompleted(event.ReceivedAt)
		case responsesws.ResponsesFailedTerminal:
			a.turns.pending.attempt.MarkCompleted(event.ReceivedAt)
		case responsesws.ResponsesCancelledTerminal:
			a.turns.pending.attempt.MarkCompleted(event.ReceivedAt)
		}
		if classified.Kind == responsesws.ResponsesSuccessTerminal ||
			classified.Kind == responsesws.ResponsesFailedTerminal ||
			classified.Kind == responsesws.ResponsesCancelledTerminal {
			a.turns.pending.attempt.MarkProviderTerminalEvidence(classified)
			return &responsesWSPendingBufferedTerminal{
				event:                       event,
				payload:                     payload,
				classified:                  classified,
				candidate:                   a.turns.pending.attempt.Candidate,
				ownerChannelID:              a.turns.pending.attempt.SelectedChannelID,
				attemptedPreviousResponseID: a.turns.pending.attempt.AttemptedPreviousResponseID,
			}, true
		}
	}
	return nil, false
}

func (a *ResponsesWSSessionActor) applyBufferedPendingTerminalSideEffects(terminal *responsesWSPendingBufferedTerminal) {
	if a == nil || terminal == nil {
		return
	}
	classified := terminal.classified
	active := CommitResponsesTurnAffinity(terminal.candidate, terminal.ownerChannelID)
	if classified.Response != nil {
		a.rememberFinalizedResponseID(classified.Response.ID)
	}
	switch classified.Kind {
	case responsesws.ResponsesSuccessTerminal:
		a.processProviderPayloadAPIError(terminal.payload, terminal.event.ChannelID, "responses_ws_provider_frame")
		RecordResponsesTurnSuccess(a.Context(), active, classified.Response)
		a.turns.history.lastFinal = classified.Response
	case responsesws.ResponsesFailedTerminal:
		a.processProviderPayloadAPIError(terminal.payload, terminal.event.ChannelID, "responses_ws_provider_frame")
		if classified.ContinuationMiss {
			a.applyContinuationMissSideEffects(active, terminal.ownerChannelID, terminal.attemptedPreviousResponseID)
		}
	case responsesws.ResponsesCancelledTerminal:
		a.processProviderPayloadAPIError(terminal.payload, terminal.event.ChannelID, "responses_ws_provider_frame")
	}
}

func (a *ResponsesWSSessionActor) applyBufferedPendingProviderFailureEvidence() bool {
	if a == nil || a.turns.pending.attempt == nil || len(a.turns.pending.provider.journal.Replay()) == 0 {
		return false
	}
	for _, entry := range a.turns.pending.provider.journal.Replay() {
		if entry.Failure == nil {
			continue
		}
		event := *entry.Failure
		if len(responsesWSProviderRecvFailureClientPayload(event)) == 0 {
			continue
		}
		a.turns.pending.attempt.MarkCompleted(event.ReceivedAt)
		return true
	}
	return false
}

func (a *ResponsesWSSessionActor) applyBufferedPendingProviderFailureSideEffects() bool {
	if a == nil || len(a.turns.pending.provider.journal.Replay()) == 0 {
		return false
	}
	for _, entry := range a.turns.pending.provider.journal.Replay() {
		if entry.Failure == nil {
			continue
		}
		event := *entry.Failure
		payload := responsesWSProviderRecvFailureClientPayload(event)
		if len(payload) == 0 {
			continue
		}
		a.writeProxyLocal(payload)
		return true
	}
	return false
}

func (a *ResponsesWSSessionActor) failClosed(reason string) {
	if a == nil || a.closing.closed.Load() {
		return
	}
	a.writeProxyLocal(responsesWSErrorPayload(http.StatusBadGateway, "responses_ws_protocol_violation", reason))
	a.close(reason)
}

func (a *ResponsesWSSessionActor) close(reason string) {
	if a == nil || a.closing.closed.Swap(true) {
		return
	}
	effectiveReason := strings.TrimSpace(reason)
	if effectiveReason == "" {
		effectiveReason = "session_closed"
	}
	a.stopActiveTurnWatchdog()
	a.cancelSetup()
	a.releasePendingLease()
	defer a.releaseActiveLease()
	effectiveReason = a.settlePendingAttemptOnClose(effectiveReason)
	a.clearPendingCreateCancel("")
	if a.turns.active.attempt != nil {
		if err := a.finalizeActiveAttempt(); err != nil {
			effectiveReason = a.handleSettlementFailureDuringClose(err, "active")
		}
	}
	a.logClose(effectiveReason)
	if a.upstream.session != nil && a.io.bridge != nil {
		a.io.bridge.AbortSession(a.upstream.session, effectiveReason)
	}
	if a.io.bridge != nil && a.io.bridge.writer != nil {
		if !a.closing.downstreamCloseSent.Swap(true) {
			a.io.bridge.WriteCloseControl(int(wsconn.CloseNormalClosure), responsesWSCloseReason(effectiveReason))
		}
	}
	a.state = responsesWSStateClosed
	a.finish()
}

func (a *ResponsesWSSessionActor) markDownstreamCloseSent() {
	if a != nil {
		a.closing.downstreamCloseSent.Store(true)
	}
}

func (a *ResponsesWSSessionActor) settlePendingAttemptOnClose(effectiveReason string) string {
	if a == nil || a.turns.pending.attempt == nil {
		return effectiveReason
	}
	terminal, hasTerminal := a.applyBufferedPendingTerminalEvidence()
	if !hasTerminal {
		a.applyBufferedPendingProviderFailureEvidence()
	}
	if a.turns.pending.attempt.RolledBack || a.turns.pending.attempt.QuotaFinalized {
		if hasTerminal && a.turns.pending.attempt.QuotaFinalized {
			a.applyBufferedPendingTerminalSideEffects(terminal)
		} else if !hasTerminal && a.turns.pending.attempt.QuotaFinalized {
			a.applyBufferedPendingProviderFailureSideEffects()
		}
		a.clearPendingTurnAfterClose()
		return effectiveReason
	}
	if proof, ok := a.pendingAttemptRollbackProofOnClose("session_closed_before_send"); ok {
		input := a.buildPendingSettlementInput("session_closed_before_send", proof)
		decision, _, err := a.applyPendingSettlement(input)
		if err != nil {
			return a.handleSettlementFailureDuringClose(err, "pending")
		}
		if responsesWSSettlementHasFlag(decision, ResponsesWSSettlementFlagContradictoryInput) {
			a.observeSettlementConflict(string(ResponsesWSSettlementFlagContradictoryInput), decision)
		}
		if hasTerminal {
			a.applyBufferedPendingTerminalSideEffects(terminal)
		} else {
			a.applyBufferedPendingProviderFailureSideEffects()
		}
		a.clearPendingTurnAfterClose()
		return effectiveReason
	}
	input := a.buildPendingSettlementInput("session_closed", ResponsesWSZeroChargeProof{})
	decision, _, err := a.applyPendingSettlement(input)
	if err != nil {
		return a.handleSettlementFailureDuringClose(err, "pending")
	}
	if responsesWSSettlementHasFlag(decision, ResponsesWSSettlementFlagContradictoryInput) {
		a.observeSettlementConflict(string(ResponsesWSSettlementFlagContradictoryInput), decision)
	}
	if hasTerminal {
		a.applyBufferedPendingTerminalSideEffects(terminal)
	} else {
		a.applyBufferedPendingProviderFailureSideEffects()
	}
	a.clearPendingTurnAfterClose()
	return effectiveReason
}

func (a *ResponsesWSSessionActor) clearPendingTurnAfterClose() {
	if a == nil {
		return
	}
	a.clearPendingTurn("session_closed")
}

func (a *ResponsesWSSessionActor) handleSettlementFailureDuringClose(err error, kind string) string {
	if a == nil {
		return "quota_settlement_failed"
	}
	if err != nil {
		a.logErrorf("responses websocket close %s settlement failed: %v", strings.TrimSpace(kind), err)
	}
	a.writeProxyLocal(responsesWSErrorPayload(http.StatusInternalServerError, "quota_settlement_failed", responsesWSStaticErrorMessage("quota_settlement_failed")))
	return "quota_settlement_failed"
}

func (a *ResponsesWSSessionActor) pendingAttemptRollbackProofOnClose(reason string) (ResponsesWSZeroChargeProof, bool) {
	if a == nil || a.turns.pending.attempt == nil {
		return ResponsesWSZeroChargeProof{}, false
	}
	if a.pendingBridgeOpenProviderErrorRecorded() {
		return responsesWSZeroChargeProof(ResponsesWSZeroChargeProofProviderRejectedBeforeStream, reason), true
	}
	switch responsesWSTransportSendStatus(a.turns.pending.attempt.TransportResult) {
	case responsesws.ResponsesWSTransportSendRejectedBeforeStream:
		return responsesWSZeroChargeProof(ResponsesWSZeroChargeProofProviderRejectedBeforeStream, reason), true
	case responsesws.ResponsesWSTransportSendNotAttempted:
		return responsesWSZeroChargeProof(ResponsesWSZeroChargeProofTransportNotAttempted, reason), true
	}
	if a.hasPendingProviderEvidence() {
		return ResponsesWSZeroChargeProof{}, false
	}
	if responsesWSTransportSendStatus(a.turns.pending.attempt.TransportResult) == "" &&
		a.turns.pending.phase != responsesWSPendingTurnSend &&
		a.state != responsesWSStatePendingSend {
		return responsesWSZeroChargeProof(ResponsesWSZeroChargeProofTransportNotAttempted, reason), true
	}
	return ResponsesWSZeroChargeProof{}, false
}

func responsesWSCloseReason(reason string) string {
	return wsconn.SafeCloseReason(strings.TrimSpace(reason))
}

func ResponsesWebSocket(c *gin.Context) {
	if apiErr := validateRealtimeWebSocketOrigin(c.Request); apiErr != nil {
		common.AbortWithErr(c, apiErr.StatusCode, apiErr)
		return
	}
	if apiErr := middleware.EnsureCurrentUserRequestAllowed(c); apiErr != nil {
		common.AbortWithMessage(c, apiErr.StatusCode, apiErr.Message)
		return
	}
	if !wsconn.IsUpgrade(c.Request) {
		common.AbortWithMessage(c, http.StatusUpgradeRequired, "websocket_upgrade_required")
		return
	}
	if apiErr := middleware.AllowResponsesWSConnectionAttempt(c); apiErr != nil {
		common.AbortWithMessage(c, apiErr.StatusCode, apiErr.Message)
		return
	}
	markResponsesWSStreamRequest(c)
	pendingLease, apiErr := middleware.AcquireResponsesWSPendingSlot(c)
	if apiErr != nil {
		common.AbortWithMessage(c, apiErr.StatusCode, apiErr.Message)
		return
	}
	defer pendingLease.Release()

	clientConn, err := wsconn.AcceptManaged(c.Writer, c.Request, responsesWSClientWSConfig(), wsconn.AcceptOptions{
		CheckOrigin:       realtimeWebSocketOriginAllowed,
		ResponseHeader:    websocketUpgradeResponseHeader(c.Request),
		EnableCompression: false,
		Subprotocols:      echoableClientWebSocketSubprotocols(c.Request),
	})
	if err != nil {
		common.AbortWithMessage(c, http.StatusInternalServerError, "upgrade_failed")
		return
	}
	firstCtx, cancelFirstFrame := context.WithTimeout(c.Request.Context(), config.ResponsesWSFirstFrameTimeout())
	mt, raw, err := clientConn.ReadInitial(firstCtx)
	cancelFirstFrame()
	firstFrameReceivedAt := time.Now()
	if err != nil {
		_ = clientConn.WriteMessage(wsconn.TextMessage, responsesWSErrorPayload(http.StatusBadRequest, "invalid_event", responsesWSFirstFrameReadErrorMessage(err)))
		logger.LogWarn(c.Request.Context(), fmt.Sprintf("responses websocket first frame read failed: err=%v timeout=%s", err, config.ResponsesWSFirstFrameTimeout()))
		clientConn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "first_frame_read_failed", Err: err})
		return
	}
	if mt != wsconn.TextMessage {
		logger.LogWarn(c.Request.Context(), fmt.Sprintf("responses websocket first frame rejected: message_type=%d reason=text_only", mt))
		clientConn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindNormal, Code: wsconn.CloseUnsupportedData, Reason: "text_only"})
		return
	}
	frame, err := responsesws.ParseRawResponsesCreateFrame(raw)
	if err != nil {
		_ = clientConn.WriteMessage(wsconn.TextMessage, responsesWSErrorPayload(http.StatusBadRequest, responsesWSErrorCodeInvalidResponseCreate, responsesWSMessageInvalidResponseCreate))
		logger.LogWarn(c.Request.Context(), fmt.Sprintf("responses websocket first frame parse failed: err=%v payload_bytes=%d", err, len(raw)))
		clientConn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindNormal, Code: wsconn.ClosePolicyViolation, Reason: responsesWSErrorCodeInvalidResponseCreate, Err: err})
		return
	}
	logResponsesWSFirstFrame(c.Request.Context(), responsesWSFrameDiagnosticsFromRaw(raw))

	actor := NewResponsesWSSessionActor(c)
	defer func() {
		if recovered := recover(); recovered != nil {
			actor.requestCloseIntent("handler_panic")
			select {
			case <-actor.Done():
			case <-time.After(responsesWSHandlerPanicCleanupGraceTime):
				actor.releasePendingLease()
				actor.releaseActiveLease()
			}
			panic(recovered)
		}
	}()
	bridge := NewResponsesWSManagedBridge(clientConn, actor)
	defer bridge.Close()
	actor.SetBridge(bridge)
	actor.SetClientConn(clientConn)
	actor.Start()
	defer armResponsesWSMaxLifetime(actor)()
	pump := wsconn.Pump{
		Conn:    clientConn,
		Handle:  actor.onClientFrame,
		OnClose: actor.onClientConnClosed,
	}
	go pump.Run(c.Request.Context())
	if !actor.PostReliable(ResponsesWSEventFirstTurnSetup{Frame: frame, PendingLease: pendingLease, ReceivedAt: firstFrameReceivedAt}) {
		pendingLease.Release()
		actor.requestCloseIntent("first_turn_setup_not_queued")
	}
	<-actor.Done()
}

func armResponsesWSMaxLifetime(actor *ResponsesWSSessionActor) func() {
	if actor == nil {
		return func() {}
	}
	maxLifetime := config.ResponsesWSMaxLifetime()
	if maxLifetime <= 0 {
		return func() {}
	}
	timer := time.AfterFunc(maxLifetime, func() {
		if !actor.PostReliable(ResponsesWSEventTimeout{Reason: "max_lifetime"}) {
			return
		}
	})
	return func() {
		timer.Stop()
	}
}
