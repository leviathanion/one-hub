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
	"one-api/common/providerresponse"
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
	"sync/atomic"
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
	TransportAttemptID        string
	ResponseID                string
	UpstreamSessionGeneration string
	SelectedChannelID         int
	Purpose                   ResponsesWSSendPurpose
	Session                   responsesws.Upstream
	Frame                     responsesws.Frame
	Context                   context.Context
	Receipt                   *responsesWSSendCompletion
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
	return hex.EncodeToString(sum[:16])
}

type ResponsesWSIOPump struct {
	ctx    context.Context
	cancel context.CancelFunc
	writer responsesWSClientWriter
	actor  *ResponsesWSSessionActor
	armed  sync.Map
	wg     sync.WaitGroup
}

func NewResponsesWSManagedPump(conn *wsconn.ManagedConn, actor *ResponsesWSSessionActor) *ResponsesWSIOPump {
	ctx, cancel := context.WithCancel(context.Background())
	pump := &ResponsesWSIOPump{
		ctx:    ctx,
		cancel: cancel,
		writer: newResponsesWSManagedClientWriter(conn),
		actor:  actor,
	}
	return pump
}

func newResponsesWSPumpForTest(writer responsesWSClientWriter, actor *ResponsesWSSessionActor) *ResponsesWSIOPump {
	ctx, cancel := context.WithCancel(context.Background())
	return &ResponsesWSIOPump{
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

func (b *ResponsesWSIOPump) ArmProviderRecvPump(upstreamSessionGeneration string, selectedChannelID int, session responsesws.Upstream) {
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
					Fallback:                  responsesWSProviderRecvErrorFallbackRecvFailed,
				}) {
					return
				}
				return
			}
			if event.Err != nil {
				if !b.postProviderFrameClientErrorAndFallback(responsesWSProviderRecvErrorDispatch{
					UpstreamSessionGeneration: upstreamSessionGeneration,
					ChannelID:                 selectedChannelID,
					Event:                     event,
					Err:                       event.Err,
					ReceivedAt:                receivedAt,
					Fallback:                  responsesWSProviderRecvErrorFallbackLifecycleOrBusiness,
				}) {
					return
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
	Fallback                  responsesWSProviderRecvErrorFallback
}

func (b *ResponsesWSIOPump) postProviderFrameClientErrorAndFallback(input responsesWSProviderRecvErrorDispatch) bool {
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
				nil,
				false,
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

func (b *ResponsesWSIOPump) postProviderRecvFailed(input responsesWSProviderRecvErrorDispatch) bool {
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
		a.workers.runWG.Add(1)
		go func() {
			defer a.workers.runWG.Done()
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

func (a *ResponsesWSSessionActor) handleSendCommand(command responsesWSSendCommand) {
	defer a.workers.sendBytes.Add(-int64(command.Frame.PayloadLen()))
	defer recoverResponsesWSGoroutine("send_command", func(reason string) {
		if a != nil {
			if !a.postTransportSendResult(command, responsesws.ResponsesWSTransportSendResult{
				Status: responsesws.ResponsesWSTransportSendAmbiguous,
				Err:    errors.New(reason),
			}) {
				return
			}
		}
	})
	select {
	case <-a.done:
		if a != nil {
			if !a.postTransportSendResult(command, responsesws.ResponsesWSTransportSendResult{
				Status: responsesws.ResponsesWSTransportSendNotAttempted,
				Err:    responsesws.ErrUpstreamClosed,
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
	sendResult := a.sendResultForCommand(ctx, command)
	if a != nil {
		if !a.postTransportSendResult(command, sendResult) {
			return
		}
	}
}

func (a *ResponsesWSSessionActor) sendResultForCommand(ctx context.Context, command responsesWSSendCommand) responsesws.ResponsesWSTransportSendResult {
	// Commands can wait in the bounded send queue. Re-check the client lifetime
	// at worker execution time so a close observed after enqueue does not start
	// new provider work.
	if a == nil || !a.allowNewWork() {
		return responsesws.ResponsesWSTransportSendResult{
			Status: responsesws.ResponsesWSTransportSendNotAttempted,
			Err:    context.Canceled,
		}
	}
	select {
	case <-ctx.Done():
		return responsesws.ResponsesWSTransportSendResult{
			Status: responsesws.ResponsesWSTransportSendNotAttempted,
			Err:    ctx.Err(),
		}
	default:
	}
	return responsesWSSendResultForCommand(ctx, command)
}

func (a *ResponsesWSSessionActor) postTransportSendResult(command responsesWSSendCommand, result responsesws.ResponsesWSTransportSendResult) bool {
	if a == nil {
		return false
	}
	if command.Purpose == ResponsesWSSendPurposeResponseInject || command.Purpose == ResponsesWSSendPurposeResponseSteer {
		if result.Status == responsesws.ResponsesWSTransportSendAmbiguous || responsesws.ValidateResponsesWSTransportSendResult(result) != nil {
			a.stopNewWork()
		}
	}
	event := ResponsesWSEventSendResult{
		Completion:                command.Receipt,
		AttemptID:                 command.AttemptID,
		ResponseID:                command.ResponseID,
		UpstreamSessionGeneration: command.UpstreamSessionGeneration,
		SelectedChannelID:         command.SelectedChannelID,
		Purpose:                   command.Purpose,
		TransportResult:           result,
	}

	if err := responsesws.ValidateResponsesWSTransportSendResult(result); err != nil {
		if result.Err != nil {
			err = errors.Join(result.Err, err)
		}
		return a.PostReliable(ResponsesWSEventTransportContractViolation{
			Completion:                command.Receipt,
			AttemptID:                 command.AttemptID,
			ResponseID:                command.ResponseID,
			UpstreamSessionGeneration: command.UpstreamSessionGeneration,
			SelectedChannelID:         command.SelectedChannelID,
			Purpose:                   command.Purpose,
			TransportResult:           result,
			Err:                       err,
		})
	}
	return a.PostReliable(event)
}

func responsesWSSendResultForCommand(ctx context.Context, command responsesWSSendCommand) responsesws.ResponsesWSTransportSendResult {
	if command.Session == nil {
		return responsesws.ResponsesWSTransportSendResult{Status: responsesws.ResponsesWSTransportSendNotAttempted, Err: responsesws.ErrUpstreamClosed}
	}
	transportAttemptID := command.TransportAttemptID
	if transportAttemptID == "" {
		transportAttemptID = command.AttemptID
	}
	return command.Session.SendClientWithResult(ctx, responsesws.SendRequest{
		AttemptID: transportAttemptID,
		Frame:     command.Frame,
	})
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
	if a == nil || session == nil || !a.allowNewWork() {
		return false
	}
	a.startSendWorker()
	ctx := context.Background()
	if actorCtx := a.Context(); actorCtx != nil && actorCtx.Request != nil {
		ctx = actorCtx.Request.Context()
	}
	command := responsesWSSendCommand{
		AttemptID:                 attemptID,
		UpstreamSessionGeneration: a.upstream.sessionGeneration,
		SelectedChannelID:         selectedChannelID,
		Purpose:                   ResponsesWSSendPurposeResponseCreate,
		Session:                   session,
		Frame:                     frame,
		Context:                   ctx,
	}
	return a.enqueueProviderSend(command)
}

func (a *ResponsesWSSessionActor) SendProviderAuxiliaryFrame(attemptID string, selectedChannelID int, session responsesws.Upstream, frame responsesws.Frame, purpose ResponsesWSSendPurpose) bool {
	if a == nil || session == nil || strings.TrimSpace(attemptID) == "" || purpose == "" {
		return false
	}
	a.startSendWorker()
	ctx := context.Background()
	if actorCtx := a.Context(); actorCtx != nil && actorCtx.Request != nil {
		ctx = actorCtx.Request.Context()
	}
	command := responsesWSSendCommand{
		AttemptID:                 strings.TrimSpace(attemptID),
		UpstreamSessionGeneration: a.upstream.sessionGeneration,
		SelectedChannelID:         selectedChannelID,
		Purpose:                   purpose,
		Session:                   session,
		Frame:                     frame,
		Context:                   ctx,
	}
	command.Receipt = &responsesWSSendCompletion{}
	if purpose == ResponsesWSSendPurposeResponseSteer {
		if work := a.observation.byAttempt(attemptID); work != nil {
			command.ResponseID = work.successorParent
		}
	}
	if !a.enqueueProviderSend(command) {
		return false
	}
	a.noteAuxiliaryCommand(attemptID, frame, purpose)
	return true
}

func (a *ResponsesWSSessionActor) enqueueProviderSend(command responsesWSSendCommand) bool {
	if a == nil || command.Frame.IsZero() || !a.allowNewWork() {
		return false
	}
	bytes := int64(command.Frame.PayloadLen())
	for {
		current := a.workers.sendBytes.Load()
		if bytes > int64(responsesWSSendQueueMaxBytes)-current {
			return false
		}
		if a.workers.sendBytes.CompareAndSwap(current, current+bytes) {
			break
		}
	}
	select {
	case <-a.done:
		a.workers.sendBytes.Add(-bytes)
		return false
	case a.workers.sendCommands <- command:
		return true
	default:
		a.workers.sendBytes.Add(-bytes)
		return false
	}
}

func (b *ResponsesWSIOPump) WriteClientFrame(mt int, payload []byte, mode ResponsesWSWriteMode) error {
	if b == nil || len(payload) == 0 {
		return nil
	}
	if b.writer == nil {
		return nil
	}
	return b.writer.WriteFrame(mt, payload, mode)
}

func (b *ResponsesWSIOPump) WriteClientTypedFrame(frame responsesws.Frame, mode ResponsesWSWriteMode) error {
	if frame.IsZero() {
		return nil
	}
	messageType := responsesWSTextMessageType
	if frame.Kind() == responsesws.FrameKindBinary {
		messageType = responsesWSBinaryMessageType
	}
	return b.WriteClientFrame(messageType, frame.Payload(), mode)
}

func (b *ResponsesWSIOPump) WriteCloseControl(code int, reason string) {
	if b == nil {
		return
	}
	if b.writer == nil {
		return
	}
	b.writer.CloseWithCode(code, reason)
}

func (b *ResponsesWSIOPump) AbortSession(session responsesws.Upstream, reason string) {
	if session != nil {
		session.Abort(strings.TrimSpace(reason))
	}
}

func (b *ResponsesWSIOPump) Close() {
	if b == nil {
		return
	}
	b.cancel()
	if b.writer != nil {
		b.writer.Abort("responses_ws_pump_closed")
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
	diag.EventType = responsesWSSafeDiagnosticValue(jsonStringField(object, "type"))
	diag.Model = responsesWSSafeDiagnosticValue(jsonStringField(object, "model"))
	diag.PreviousResponse = responsesWSSafeDiagnosticValue(jsonStringField(object, "previous_response_id"))
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
			diag.TurnRequestKind = responsesWSSafeDiagnosticValue(responsesWSTurnRequestKind(turnMetadata))
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
	observation             responsesWSObservations
	heldSteeringParents     map[string]responsesWSParentProof
	heldSteeringParentBytes int

	events     chan ResponsesWSEvent
	eventSpace chan struct{}
	eventBytes atomic.Int64
	done       chan struct{}
	doneOnce   sync.Once

	io       responsesWSIOState
	snapshot responsesWSSnapshotState
	lease    responsesWSLeaseState
	upstream responsesWSUpstreamState

	turns         responsesWSTurnSlots
	workers       responsesWSWorkerState
	closing       responsesWSCloseState
	watchdog      responsesWSWatchdogState
	downstreamSeq uint64

	state               responsesWSSessionState
	reliablePostTimeout time.Duration
}

func NewResponsesWSSessionActor(c *gin.Context) *ResponsesWSSessionActor {
	actor := &ResponsesWSSessionActor{
		events:     make(chan ResponsesWSEvent, responsesWSEventQueueSize),
		eventSpace: make(chan struct{}, 1),
		done:       make(chan struct{}),
		workers: responsesWSWorkerState{
			sendCommands: make(chan responsesWSSendCommand, responsesWSSendQueueSize),
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

func (a *ResponsesWSSessionActor) SetPump(pump *ResponsesWSIOPump) {
	a.io.pump = pump
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
	if a == nil || a.closing.closed.Load() || a.closing.ingressClosed.Load() {
		return false
	}
	if a.tryPostEvent(event) {
		return true
	}
	a.postEventBackpressureTimeout()
	return false
}

func (a *ResponsesWSSessionActor) tryPostEvent(event ResponsesWSEvent) bool {
	if a == nil || a.closing.closed.Load() || a.closing.ingressClosed.Load() || !a.reserveEventBytes(event) {
		return false
	}
	posted, _ := a.tryEnqueueReservedEvent(event)
	if posted {
		return true
	}
	a.releaseEventBytes(event)
	return false
}

func (a *ResponsesWSSessionActor) postEventBackpressureTimeout() {
	if a == nil || !a.closing.backpressurePosted.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer recoverResponsesWSGoroutine("backpressure_post", nil)
		if ok, timedOut := a.postEventBounded(ResponsesWSEventTimeout{Reason: "responses_ws_event_backpressure"}, responsesWSBackpressurePostTimeout); !ok && timedOut {
			a.logErrorf("responses websocket backpressure timeout post timed out")
		}
	}()
}

func (a *ResponsesWSSessionActor) tryEnqueueReservedEvent(event ResponsesWSEvent) (bool, bool) {
	if a == nil {
		return false, true
	}
	a.closing.postMu.Lock()
	defer a.closing.postMu.Unlock()
	if a.closing.closed.Load() || a.closing.ingressClosed.Load() {
		return false, true
	}
	select {
	case a.events <- event:
		return true, false
	default:
		return false, false
	}
}

func (a *ResponsesWSSessionActor) signalEventSpace() {
	if a == nil {
		return
	}
	select {
	case a.eventSpace <- struct{}{}:
	default:
	}
}

func (a *ResponsesWSSessionActor) reserveEventBytes(event ResponsesWSEvent) bool {
	bytes := int64(responsesWSEventPayloadBytes(event))
	if bytes <= 0 {
		return true
	}
	for {
		current := a.eventBytes.Load()
		if bytes > responsesWSEventQueueMaxBytes-current {
			return false
		}
		if a.eventBytes.CompareAndSwap(current, current+bytes) {
			return true
		}
	}
}

func (a *ResponsesWSSessionActor) releaseEventBytes(event ResponsesWSEvent) {
	if bytes := int64(responsesWSEventPayloadBytes(event)); bytes > 0 {
		a.eventBytes.Add(-bytes)
	}
}

func (a *ResponsesWSSessionActor) PostReliable(event ResponsesWSEvent) bool {
	if a == nil || a.closing.closed.Load() || a.closing.ingressClosed.Load() {
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
	if a == nil || a.closing.closed.Load() || a.closing.ingressClosed.Load() {
		return false, false
	}
	if !a.reserveEventBytes(event) {
		return false, true
	}
	posted := false
	defer func() {
		if !posted {
			a.releaseEventBytes(event)
		}
	}()
	if ok, closed := a.tryEnqueueReservedEvent(event); ok {
		posted = true
		return true, false
	} else if closed {
		return false, false
	}
	if timeout <= 0 {
		return false, true
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	poll := time.NewTicker(time.Millisecond)
	defer poll.Stop()
	for {
		select {
		case <-a.done:
			return false, false
		case <-timer.C:
			return false, true
		case <-a.eventSpace:
		case <-poll.C:
		}
		if ok, closed := a.tryEnqueueReservedEvent(event); ok {
			posted = true
			return true, false
		} else if closed {
			return false, false
		}
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
		return errors.New("responses websocket attempt is required")
	}
	if a.observation.byAttempt(attempt.AttemptID) != nil {
		return nil
	}
	attempt.initializeIdentity()
	if err := a.observation.add(attempt, responsesWSFrameLane(attempt.RequestFrame), ""); err != nil {
		return err
	}
	// pending is only the synchronous preparation cleanup slot, not upstream state.
	a.turns.pending.attempt = attempt
	a.turns.pending.openingID = attempt.OpeningID
	return nil

}

func (a *ResponsesWSSessionActor) MarkPendingSend() {

	a.turns.pending = responsesWSPendingTurn{}
	a.state = responsesWSStateIdle

}

func (a *ResponsesWSSessionActor) settlePendingAttemptBeforeLocalWrite(reason string) error {
	if a == nil {
		return nil
	}
	attempt := a.turns.pending.attempt
	if attempt != nil {
		_, _, err := a.applyPendingSettlement()
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

func (a *ResponsesWSSessionActor) settlePendingAttemptOrClose(reason string) bool {
	if err := a.settlePendingAttemptBeforeLocalWrite(reason); err != nil {
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
	a.closing.postMu.Lock()
	a.closing.clientClosed.Store(true)
	a.closing.workStopped.Store(true)
	a.closing.postMu.Unlock()
	a.cancelSetup()
	if a.io.pump != nil && a.io.pump.cancel != nil {
		a.io.pump.cancel()
	}
}

// 与关闭发布共用短锁；许可只适用于锁外紧邻的一次操作，不跨 SQL/I/O 持锁。
func (a *ResponsesWSSessionActor) allowNewWork() bool {
	if a == nil {
		return false
	}
	a.closing.postMu.Lock()
	defer a.closing.postMu.Unlock()
	return !a.closing.workStopped.Load() && !a.closing.closed.Load() && !a.closing.ingressClosed.Load() && !a.closing.clientClosed.Load() && !a.closing.closeIntentPosted.Load()
}

func (a *ResponsesWSSessionActor) stopNewWork() {
	if a == nil {
		return
	}
	a.closing.postMu.Lock()
	a.closing.workStopped.Store(true)
	a.closing.postMu.Unlock()
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

func (a *ResponsesWSSessionActor) releasePendingLease() {
	if a == nil {
		return
	}
	a.lease.mu.Lock()
	lease := a.lease.pendingLease
	bytes := a.lease.pendingBytes
	a.lease.pendingLease = nil
	a.lease.pendingBytes = nil
	a.lease.mu.Unlock()
	if lease != nil {
		lease.Release()
	}
	if bytes != nil {
		bytes.Release()
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

func (a *ResponsesWSSessionActor) setPendingBytes(lease middleware.ResponsesWSByteLease) {
	if a == nil {
		return
	}
	a.lease.mu.Lock()
	a.lease.pendingBytes = lease
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
			a.releaseEventBytes(event)
			a.signalEventSpace()
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
			}
		}
	}
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
	case ResponsesWSEventProxyLocalError:
		if typed.UpstreamSessionGeneration != "" && typed.UpstreamSessionGeneration != a.upstream.sessionGeneration {
			return
		}
		if typed.ChannelID > 0 && typed.ChannelID != a.upstream.channelID {
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
	if event.Reason == "idle_timeout" && a.isBusy() {
		// Idle cleanup is connection cleanup, not an active-turn deadline.
		a.markActivity()
		return
	}
	a.close(event.Reason)
}

func (a *ResponsesWSSessionActor) handleFirstTurnSetup(event ResponsesWSEventFirstTurnSetup) {
	if a == nil {
		return
	}
	a.setPendingLease(event.PendingLease)
	a.setPendingBytes(event.PendingBytes)
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
	if !a.allowNewWork() {
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
	if !a.allowNewWork() {
		a.close("client_closed_before_active_lease")
		return
	}
	if !a.allowNewWork() {
		a.releasePendingLease()
		a.close("client_closed_before_upstream_open")
		return
	}

	a.startFirstTurnOpenWorker(openingID, event.Frame)
}

func (a *ResponsesWSSessionActor) startFirstTurnOpenWorker(openingID string, frame *responsesws.RawResponsesCreateFrame) {
	if a == nil || frame == nil || !a.allowNewWork() {
		return
	}
	admission := a.turns.opening.admission
	if admission == nil {
		admission = NewResponsesWSTurnAdmission()
		a.turns.opening.admission = admission
	}
	setupCtx, cancel := context.WithCancel(context.WithValue(context.Background(), responsesWSOpenPermitKey{}, a.allowNewWork))
	a.setSetupCancel(cancel)
	actorSnapshot := a.snapshotClone()

	a.workers.runWG.Add(1)
	go func() {
		defer a.workers.runWG.Done()
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
		if !a.allowNewWork() {
			return
		}
		if resourceErr := prepareResourceRequest(actorContext, frame.Raw, "responses"); resourceErr != nil {
			apiErr = common.ErrorWrapperLocal(resourceErr, "resource_not_authorized", http.StatusForbidden)
		} else if principalErr := middleware.RefreshAuthenticatedLongLivedPrincipal(actorContext); principalErr != nil {
			apiErr = principalErr
		} else {
			openResult, apiErr = openAndPrimeResponsesWSSessionForActor(setupCtx, actorContext, frame, &frame.Projection, func(openContext *gin.Context) (middleware.ResponsesWSLease, *types.OpenAIErrorWithStatusCode) {
				if !a.allowNewWork() {
					return nil, common.ErrorWrapperLocal(context.Canceled, "responses_ws_closing", http.StatusServiceUnavailable)
				}
				activeLease, leaseErr := middleware.AcquireResponsesWSActiveLease(openContext)
				if leaseErr != nil {
					return nil, leaseErr
				}
				if rpmErr := admission.AllowRPMOnce(func() *types.OpenAIErrorWithStatusCode {
					if !a.allowNewWork() {
						return common.ErrorWrapperLocal(context.Canceled, "responses_ws_closing", http.StatusServiceUnavailable)
					}
					return middleware.AllowCurrentUserRequest(openContext)
				}); rpmErr != nil {
					activeLease.Release()
					return nil, rpmErr
				}
				return activeLease, nil
			})
		}
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
		if a.PostReliable(event) {
			handedOff = true
		} else if setupCtx.Err() != nil {
			cleanupResponsesWSOpenResult(openResult, "first_turn_open_cancelled")
			return
		} else {
			cleanupResponsesWSOpenResult(openResult, "first_turn_open_abandoned")
			return
		}

		// 入队成功后的结果一定由 actor 或 closure cut 决定责任。done 不转移清理权。
		if ok := <-adopted; !ok {
			cleanupResponsesWSOpenResult(openResult, "first_turn_open_not_adopted")
		}
	}()
}

func cleanupResponsesWSOpenResult(openResult *responsesWSOpenResult, reason string) {
	if openResult == nil {
		return
	}
	if openResult.Session != nil {
		session := openResult.Session
		openResult.Session = nil
		session.Abort(strings.TrimSpace(reason))
	}
	if openResult.ActiveLease != nil {
		lease := openResult.ActiveLease
		openResult.ActiveLease = nil
		lease.Release()
	}
}

func (a *ResponsesWSSessionActor) handleFirstTurnOpenResult(event ResponsesWSEventFirstTurnOpenResult) {
	if a == nil {
		if event.Adopted != nil {
			event.Adopted <- false
		}
		return
	}
	// 先登记清理责任，再宣布接管；异常退出同样不会把资源留给已退出的 worker。
	defer cleanupResponsesWSOpenResult(event.OpenResult, "first_turn_open_untransferred")
	if event.Adopted != nil {
		event.Adopted <- true
	}
	defer a.clearSetupCancel()
	if event.OpeningID == "" || event.OpeningID != a.turns.opening.openingID || a.turns.pending.phase != responsesWSPendingTurnOpening {
		cleanupResponsesWSOpenResult(event.OpenResult, "stale_first_turn_open_result")
		return
	}
	if !a.allowNewWork() {
		cleanupResponsesWSOpenResult(event.OpenResult, "client_closed_during_open")
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
			if a.io.pump != nil {
				a.markDownstreamCloseSent()
				a.io.pump.WriteCloseControl(int(wsconn.CloseNormalClosure), "responses_ws_unsupported_for_channel")
			}
		} else {
			a.writeProxyLocal(responsesWSErrorFromOpenAI(event.Err))
		}
		a.close("open_failed")
		return
	}
	if event.OpenResult == nil || event.OpenResult.Session == nil || event.OpenResult.Channel == nil {
		cleanupResponsesWSOpenResult(event.OpenResult, "invalid_first_turn_open_result")
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusServiceUnavailable, "channel_error", "responses websocket open did not return a channel"))
		a.close("open_failed")
		return
	}

	if event.OpenResult.ActiveLease != nil {
		a.setActiveLease(event.OpenResult.ActiveLease)
		event.OpenResult.ActiveLease = nil
	}
	a.releasePendingLease()
	a.prepareAndSendFirstTurn(event.OpenResult)
}

func (a *ResponsesWSSessionActor) prepareAndSendFirstTurn(openResult *responsesWSOpenResult) {
	if a == nil || openResult == nil || openResult.Session == nil || openResult.Channel == nil || a.turns.opening.firstFrame == nil {
		if openResult != nil && openResult.Session != nil {
			cleanupResponsesWSOpenResult(openResult, "first_turn_prepare_failed")
		}
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusServiceUnavailable, "channel_error", "responses websocket first turn setup is incomplete"))
		a.close("first_turn_prepare_failed")
		return
	}
	if !a.allowNewWork() {
		cleanupResponsesWSOpenResult(openResult, "client_closed_before_first_turn_prepare")
		a.close("client_closed_before_first_turn_prepare")
		return
	}

	a.mutateSnapshot(func(snapshot *ResponsesWSRequestSnapshot) {
		attachResponsesWSSelectedChannelFacts(snapshot, openResult.Channel, openResult.ProviderModel)
	})
	a.upstream.provider = openResult.Provider
	actorCtx := a.Context()
	actorSnapshot := a.snapshotClone()
	session := openResult.Session
	upstreamSessionGeneration := a.AttachUpstreamSession(session, openResult.Channel.Id)
	openResult.Session = nil // session 的清理责任已交给 actor。
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
		Session:           session,
		BillingModel:      openResult.BillingModel,
		PromptModel:       openResult.ProviderModel,
		Request:           &a.turns.opening.firstFrame.Projection,
		RequestFrame:      a.turns.opening.firstFrame,
		MultiAgentEnabled: a.turns.opening.firstFrame.MultiAgentEnabled,
		StartedAt:         a.turns.opening.startedAt,
	})
	if apiErr != nil {
		a.writeProxyLocal(responsesWSErrorFromOpenAI(apiErr))
		a.close("attempt_prepare_failed")
		return
	}
	if !a.allowNewWork() {
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
		if !a.settlePendingAttemptOrClose("rewrite_failed") {
			return
		}
		a.logErrorf("responses websocket rewrite failed: %s", err.Error())
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusInternalServerError, "responses_ws_payload_rewrite_failed", responsesWSStaticErrorMessage("responses_ws_payload_rewrite_failed")))
		a.close("rewrite_failed")
		return
	}
	if !a.allowNewWork() {
		if !a.settlePendingAttemptOrClose("client_closed_before_quota_preconsume") {
			return
		}
		a.close("client_closed_before_quota_preconsume")
		return
	}
	if apiErr = a.reserveAndClaimResponsesWork(attempt); apiErr != nil {
		a.writeProxyLocal(responsesWSErrorFromOpenAI(apiErr))
		a.close("submission_admission_failed")
		return
	}

	a.MarkPendingSend()
	a.upstream.recvArmed = true
	a.io.pump.ArmProviderRecvPump(upstreamSessionGeneration, openResult.Channel.Id, session)
	if !a.SendProviderFrame(attempt.AttemptID, openResult.Channel.Id, session, responsesws.NewTextFrame(payload)) {
		a.handleSendQueueFull(attempt.AttemptID, openResult.Channel.Id)
	}
	a.startQueuedResponseCreates()
}

func (a *ResponsesWSSessionActor) handleClientFrame(event ResponsesWSEventClientFrame) {
	if !a.allowNewWork() {
		return
	}
	if event.Frame.Kind() != responsesws.FrameKindText {
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusBadRequest, "invalid_event", "only text websocket events are supported"))
		if a.io.pump != nil && a.io.pump.writer != nil && !a.closing.downstreamCloseSent.Swap(true) {
			a.io.pump.WriteCloseControl(int(wsconn.CloseUnsupportedData), "text_only")
		}
		a.close("client_non_text_frame")
		return
	}
	payload := event.Frame.Payload()
	envelope, err := responsesws.ParseClientEventEnvelope(payload)
	if err != nil {
		a.logErrorf("responses websocket client frame parse failed: %s", err.Error())
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusBadRequest, "invalid_event", responsesWSMessageInvalidWebsocketEvent))
		return
	}
	if a.upstream.session == nil && (envelope.Type == "response.create" || envelope.Type == "response.inject" || envelope.Type == "response.steer") {
		if !a.turns.queue.Push(event, responsesWSQueuedCreateMaxFrames, responsesWSQueuedCreateMaxBytes) {
			a.writeProxyLocal(responsesWSErrorPayload(http.StatusTooManyRequests, "responses_ws_turn_queue_full", "responses websocket opening queue is full"))
			a.close("responses_ws_turn_queue_full")
		}
		return
	}
	switch strings.TrimSpace(envelope.Type) {
	case "response.create":
		if _, err := responsesws.ParseRawResponsesCreateFrame(payload); err != nil {
			a.logErrorf("responses websocket response.create parse failed: %s", err.Error())
			a.writeProxyLocal(responsesWSErrorPayload(http.StatusBadRequest, responsesWSErrorCodeInvalidResponseCreate, responsesWSMessageInvalidResponseCreate))
			return
		}

		a.startSubsequentTurn(payload, event.ReceivedAt)
	case "response.inject":
		a.handleClientInject(event)
	case "response.steer":
		a.handleClientSteer(event)
	default:
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusBadRequest, "unsupported_client_event", "unsupported responses websocket client event"))
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
	if !a.tryPostEvent(event) {
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
	// Publish the transport fact before the actor event is queued. The event may
	// wait behind provider setup or another bounded operation; send workers must
	// still be able to prove immediately that no new provider work may start.
	a.markClientClosed(info.Err)
	go func() {
		defer recoverResponsesWSGoroutine("client_close_post", nil)
		if !a.PostReliable(ResponsesWSEventClientClosed{Err: info.Err}) {
			return
		}
	}()
}

func (a *ResponsesWSSessionActor) startSubsequentTurn(raw []byte, receivedAt time.Time) {
	if !a.allowNewWork() {
		return
	}
	frame, err := responsesws.ParseRawResponsesCreateFrame(raw)
	if err != nil {
		a.logErrorf("responses websocket subsequent frame parse failed: %s", err.Error())
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusBadRequest, responsesWSErrorCodeInvalidResponseCreate, responsesWSMessageInvalidResponseCreate))
		return
	}
	if len(a.observation.works) >= responsesWSObservationLimit {
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusTooManyRequests, "responses_ws_work_capacity", "responses websocket work observation capacity exceeded"))
		return
	}
	attempt := a.prepareSubsequentTurn(frame, receivedAt)
	if attempt == nil {
		return
	}
	request := frame.Projection
	ctx := a.Context()
	providerModel := request.Model
	var apiErr *types.OpenAIErrorWithStatusCode
	if err := a.BeginCandidate(attempt); err != nil {
		a.logErrorf("responses websocket attempt begin failed: %s", err.Error())
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusInternalServerError, "responses_ws_attempt_failed", responsesWSStaticErrorMessage("responses_ws_attempt_failed")))
		return
	}
	payload, err := responsesWSProviderPayload(ctx, frame, &request, providerModel)
	if err != nil {
		if !a.settlePendingAttemptOrClose("rewrite_failed") {
			return
		}
		a.logErrorf("responses websocket rewrite failed: %s", err.Error())
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusInternalServerError, "responses_ws_payload_rewrite_failed", responsesWSStaticErrorMessage("responses_ws_payload_rewrite_failed")))
		return
	}
	if apiErr = a.reserveAndClaimResponsesWork(attempt); apiErr != nil {
		if !a.settlePendingAttemptOrClose("submission_admission_failed") {
			return
		}
		a.writeProxyLocal(responsesWSErrorFromOpenAI(apiErr))
		return
	}
	a.MarkPendingSend()
	if !a.upstream.recvArmed {
		a.upstream.recvArmed = true
		a.io.pump.ArmProviderRecvPump(a.upstream.sessionGeneration, a.upstream.channelID, a.upstream.session)
	}
	if !a.SendProviderFrame(attempt.AttemptID, a.upstream.channelID, a.upstream.session, responsesws.NewTextFrame(payload)) {
		a.handleSendQueueFull(attempt.AttemptID, a.upstream.channelID)
	}
}

// prepareSubsequentTurn 在任何新上游响应可能开始前执行共享准入。
func (a *ResponsesWSSessionActor) prepareSubsequentTurn(frame *responsesws.RawResponsesCreateFrame, receivedAt time.Time) *ResponsesWSTurnAttempt {
	resourceContext := a.Context()
	if err := prepareResourceRequest(resourceContext, frame.Raw, "responses"); err != nil {
		a.writeProxyLocal(responsesWSErrorFromErr(err))
		return nil
	}
	a.RefreshContext(resourceContext)
	request := frame.Projection
	if err := validateResponsesSupportedSurface(&request, frame.Object, responsesOperationCreate); err != nil {
		a.writeProxyLocal(responsesWSErrorFromOpenAI(capabilityGateAPIError(err)))
		return nil
	}

	return a.prepareResponsesWork(frame, request, receivedAt, true)
}

// 原始 create 和原生 steering 共享代理拥有的准入规则；input schema 仍由上游解释。
func (a *ResponsesWSSessionActor) prepareResponsesWork(frame *responsesws.RawResponsesCreateFrame, request types.OpenAIResponsesRequest, receivedAt time.Time, explicitCreate bool) *ResponsesWSTurnAttempt {
	if !a.allowNewWork() {
		return nil
	}
	var err error
	request.Model = strings.TrimSpace(request.Model)
	ctx := a.Context()
	if ctx == nil {
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusInternalServerError, "responses_ws_context_missing", "responses websocket request context is unavailable"))
		return nil
	}
	if apiErr := middleware.RefreshAuthenticatedLongLivedPrincipal(ctx); apiErr != nil {
		a.writeProxyLocal(responsesWSErrorFromOpenAI(apiErr))
		a.close("principal_revalidation_failed")
		return nil
	}
	var selectedChannel *model.Channel
	if channel, ok := ctx.Get("responses_ws_selected_channel"); ok {
		selectedChannel, _ = channel.(*model.Channel)
	}
	if selectedChannel == nil && a.upstream.provider != nil {
		selectedChannel = a.upstream.provider.GetChannel()
	}
	if middleware.IsAuthenticatedLongLivedPrincipal(ctx) {
		if err := middleware.EnsureLongLivedChannelAllowed(ctx, request.Model); err != nil {
			a.writeProxyLocal(responsesWSErrorPayload(http.StatusForbidden, "permission_denied", err.Error()))
			a.close("principal_channel_permission_changed")
			return nil
		}
	}
	a.RefreshContext(ctx)
	if apiErr := responsesWSSelectedChannelModelAdmissionError(ctx, selectedChannel, request.Model, true); apiErr != nil {
		a.writeProxyLocal(responsesWSErrorPayload(apiErr.StatusCode, openAIErrorCodeString(apiErr.Code, "system_error"), apiErr.Message))
		return nil
	}
	requireStored := request.Store == nil || *request.Store
	if err := requireResponsesWSAdapterSupport(requireStored, frame.Object, request.Model)(selectedChannel); err != nil {
		if wrapped := capabilityGateAPIError(err); wrapped != nil {
			if strings.TrimSpace(wrapped.Param) == "" {
				wrapped.Code = "responses_ws_unsupported_for_channel"
			}
			a.writeProxyLocal(responsesWSErrorFromOpenAI(wrapped))
		}
		return nil
	}
	providerModel := request.Model
	billingModel := request.Model
	a.mutateSnapshot(func(snapshot *ResponsesWSRequestSnapshot) {
		snapshot.Set("original_model", request.Model)
		snapshot.Set("new_model", request.Model)
		snapshot.Set("billing_original_model", false)
		attachResponsesWSSelectedChannelFacts(snapshot, selectedChannel, providerModel)
	})
	ctx = a.Context()
	candidate, localContinuation := a.connectionLocalTurnAffinity(ctx, &request)
	if !localContinuation {
		candidate, err = PrepareResponsesTurnAffinity(ResponsesAffinityInput{Context: ctx, Request: &request})
	}
	if err != nil {
		a.logErrorf("responses websocket affinity conflict: %s", err.Error())
		if ownershipErr := responsesOwnershipAPIError(err); ownershipErr != nil {
			a.writeProxyLocal(responsesWSErrorFromOpenAI(ownershipErr))
			return nil
		}
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusConflict, "responses_affinity_conflict", responsesWSStaticErrorMessage("responses_affinity_conflict")))
		return nil
	}
	if err := responsesAffinityOwnerConflict(candidate, a.upstream.channelID); err != nil {
		a.logErrorf("responses websocket affinity owner conflict: %s", err.Error())
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusConflict, "responses_affinity_conflict", responsesWSStaticErrorMessage("responses_affinity_conflict")))
		return nil
	}

	if !a.allowNewWork() {
		return nil
	}
	admission := NewResponsesWSTurnAdmission()
	if apiErr := admission.AllowRPMOnce(func() *types.OpenAIErrorWithStatusCode {
		return middleware.AllowCurrentUserRequest(ctx)
	}); apiErr != nil {
		a.writeProxyLocal(responsesWSErrorFromOpenAI(apiErr))
		return nil
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
		RequestFrame:      frame,
		MultiAgentEnabled: frame.MultiAgentEnabled,
		StartedAt:         receivedAt,
	})
	if apiErr != nil {
		a.writeProxyLocal(responsesWSErrorFromOpenAI(apiErr))
		return nil
	}
	return attempt
}

func responsesWSChannelSupportsExactModel(channel *model.Channel, requestedModel string) bool {
	if channel == nil || strings.TrimSpace(requestedModel) == "" {
		return false
	}
	requestedModel = strings.TrimSpace(requestedModel)
	mappedModel, err := mappedModelForChannel(channel, requestedModel)
	if err != nil || strings.TrimSpace(mappedModel) != requestedModel {
		return false
	}
	for _, configured := range strings.Split(channel.Models, ",") {
		configured = strings.TrimSpace(configured)
		if configured == requestedModel || configured == "*" {
			return true
		}
		if strings.HasSuffix(configured, "*") && strings.HasPrefix(requestedModel, strings.TrimSuffix(configured, "*")) {
			return true
		}
	}
	return false
}

// responsesWSSelectedChannelModelAdmissionError 统一处理首轮选定渠道和后续轮次的模型准入。
// 绕过常规选路的固定渠道，以及后续轮次，都必须精确匹配模型并允许流式使用。
func responsesWSSelectedChannelModelAdmissionError(c *gin.Context, channel *model.Channel, requestedModel string, requireExactChannelModel bool) *types.OpenAIErrorWithStatusCode {
	if err := checkLimitModel(c, requestedModel); err != nil {
		return responsesWSLocalModelAdmissionError(http.StatusNotFound, "model_not_found", err.Error())
	}
	if !requireExactChannelModel {
		return nil
	}
	if channel == nil || !responsesWSChannelSupportsExactModel(channel, requestedModel) {
		return responsesWSLocalModelAdmissionError(http.StatusConflict, "responses_ws_model_unsupported_by_channel", "the selected responses websocket channel does not support the requested model")
	}
	if !channel.AllowStream(requestedModel) {
		return responsesWSLocalModelAdmissionError(http.StatusUpgradeRequired, "responses_ws_unsupported_for_channel", "the selected responses websocket channel does not allow streaming for the requested model")
	}
	return nil
}

func responsesWSLocalModelAdmissionError(status int, code string, message string) *types.OpenAIErrorWithStatusCode {
	return &types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{
			Message: message,
			Type:    "invalid_request_error",
			Code:    code,
		},
		StatusCode: status,
		LocalError: true,
	}
}

func (a *ResponsesWSSessionActor) handleSendQueueFull(attemptID string, selectedChannelID int) {

	if work := a.observation.byAttempt(attemptID); work != nil {
		a.finishObservedWork(work)
	}
	a.writeProxyLocal(responsesWSErrorPayload(http.StatusServiceUnavailable, "responses_ws_send_queue_full", "responses websocket send queue is full"))

}

func (a *ResponsesWSSessionActor) handleSendResult(event ResponsesWSEventSendResult) {

	if event.Purpose != "" && event.Purpose != ResponsesWSSendPurposeResponseCreate && event.Purpose != ResponsesWSSendPurposeResponseSteer && event.Purpose != ResponsesWSSendPurposeResponseInject {
		return
	}
	if event.Purpose == ResponsesWSSendPurposeResponseInject {
		a.consumeInjectSend(event)
		return
	}
	if event.UpstreamSessionGeneration != "" && event.UpstreamSessionGeneration != a.upstream.sessionGeneration {
		return
	}
	if event.SelectedChannelID != a.upstream.channelID {
		return
	}
	if event.Completion != nil {
		if event.Completion.consumed {
			return
		}
		event.Completion.consumed = true
	}
	work := a.observation.byAttempt(event.AttemptID)
	if err := responsesws.ValidateResponsesWSTransportSendResult(event.TransportResult); err != nil {
		a.failClosed("responses_ws_transport_contract_violation")
		return
	}
	if event.TransportResult.Status == responsesws.ResponsesWSTransportSendAmbiguous {
		if work != nil {
			work.attempt.CommitAmbiguousAdmission("send_ambiguous")
		}
		a.close("ambiguous_upstream_write")
		return
	}
	if work == nil {
		return
	}
	work.attempt.TransportResult = event.TransportResult
	switch event.TransportResult.Status {
	case responsesws.ResponsesWSTransportSendAttempted:
		work.attempt.CommitLocalWriteOK()
		if a.observation.unsafeLane(work.lane) && work.attempt.SeenProviderResponseID == "" {
			a.finishObservedWork(work)
		}
	case responsesws.ResponsesWSTransportSendNotAttempted:
		if event.Purpose == ResponsesWSSendPurposeResponseSteer {
			// A refused auxiliary write says nothing about earlier steer writes.
			a.abandonLaneObservation(work.lane)
		} else {
			a.finishObservedWork(work)
		}
		a.writeProxyLocal(responsesWSErrorFromErr(event.TransportResult.Err))
	case responsesws.ResponsesWSTransportSendAmbiguous:
		work.attempt.CommitAmbiguousAdmission("send_ambiguous")
		// A real uncertain write is a transport failure, never a reason to replay.
		a.close("ambiguous_upstream_write")
	}

}

func (a *ResponsesWSSessionActor) handleTransportContractViolation(event ResponsesWSEventTransportContractViolation) {

	if event.UpstreamSessionGeneration != "" && event.UpstreamSessionGeneration != a.upstream.sessionGeneration {
		return
	}
	a.failClosed("responses_ws_transport_contract_violation")

}

func (a *ResponsesWSSessionActor) rememberConnectionLocalEphemeralResponseID(responseID string) {

	if a == nil || responseID == "" || len(responseID) > responsesWSSteerMaxIDBytes {
		return
	}
	for _, current := range a.turns.history.localEphemeralResponseIDs {
		if current == responseID {
			return
		}
	}
	a.turns.history.localEphemeralResponseIDs = append(a.turns.history.localEphemeralResponseIDs, responseID)
	if len(a.turns.history.localEphemeralResponseIDs) > responsesWSRecentResponseIDLimit {
		a.turns.history.localEphemeralResponseIDs = append([]string(nil), a.turns.history.localEphemeralResponseIDs[len(a.turns.history.localEphemeralResponseIDs)-responsesWSRecentResponseIDLimit:]...)
	}
}

func (a *ResponsesWSSessionActor) connectionLocalTurnAffinity(c *gin.Context, request *types.OpenAIResponsesRequest) (*ResponsesTurnAffinity, bool) {
	if a == nil || c == nil || request == nil {
		return nil, false
	}
	responseID := request.PreviousResponseID
	if responseID == "" {
		return nil, false
	}
	found := a.hasConnectionLocalResponseProof(c, responseID)
	if !found {
		return nil, false
	}
	prepareResponsesChannelAffinity(c, request)
	return &ResponsesTurnAffinity{
		State:              currentChannelAffinityState(c),
		PreviousResponseID: responseID,
		ExplicitPinID:      explicitChannelPinID(c),
		OwnershipChannelID: a.upstream.channelID,
	}, true
}

func (a *ResponsesWSSessionActor) forgetConnectionLocalEphemeralResponseID(responseID string) {

	if a == nil || responseID == "" {
		return
	}
	for index, current := range a.turns.history.localEphemeralResponseIDs {
		if current != responseID {
			continue
		}
		copy(a.turns.history.localEphemeralResponseIDs[index:], a.turns.history.localEphemeralResponseIDs[index+1:])
		a.turns.history.localEphemeralResponseIDs[len(a.turns.history.localEphemeralResponseIDs)-1] = ""
		a.turns.history.localEphemeralResponseIDs = a.turns.history.localEphemeralResponseIDs[:len(a.turns.history.localEphemeralResponseIDs)-1]
		return
	}
}

func responsesWSProviderDownstreamResponseID(event ResponsesWSEventProviderDownstream) string {
	if event.Frame != nil && event.Frame.Kind() == responsesws.FrameKindText {
		return responsesws.ProviderResponseID(event.Frame.Payload())
	}
	if event.ResponseID != "" {
		return event.ResponseID
	}
	if event.Usage != nil {
		return event.Usage.ResponseID
	}
	return ""
}

func responsesWSProviderResponseIDsAgree(ids ...string) bool {
	known := ""
	for _, id := range ids {

		if id == "" {
			continue
		}
		if known != "" && known != id {
			return false
		}
		known = id
	}
	return true
}

func responsesWSProviderUsageResponseID(event ResponsesWSEventProviderUsageObserved) string {
	if responseID := event.ResponseID; responseID != "" {
		return responseID
	}
	if event.Usage != nil {
		return event.Usage.ResponseID
	}
	return ""
}

func responsesWSPayloadResponseID(payload []byte) string {
	return responsesws.ProviderResponseID(payload)
}

func (a *ResponsesWSSessionActor) handleProviderDownstream(event ResponsesWSEventProviderDownstream) {
	a.observeParallelDownstream(event)
}

func (a *ResponsesWSSessionActor) handleProviderUsageObserved(event ResponsesWSEventProviderUsageObserved) {

	if !a.parallelProviderSource(event.UpstreamSessionGeneration, event.ChannelID) || event.Usage == nil {
		return
	}
	id := responsesWSProviderUsageResponseID(event)
	if !responsesWSProviderResponseIDsAgree(event.ResponseID, event.Usage.ResponseID) {
		return
	}
	if work := a.observation.byResponse(id); work != nil && responsesWSProviderPayloadPolicyForEvent(upstreamEventFromProviderUsage(event)).CanCarryUsage {
		mergeResponsesWSUsageEvent(work.attempt.Usage, event.Usage)
	}

}

func (a *ResponsesWSSessionActor) handleProviderBusinessError(event ResponsesWSEventProviderBusinessError) {

	if !a.parallelProviderSource(event.UpstreamSessionGeneration, event.ChannelID) {
		return
	}
	// The adapter supplied no raw error frame. Report the available error but
	// do not turn an upstream business rejection into connection termination.
	if event.Err != nil {
		a.writeProxyLocal(responsesWSErrorFromErr(event.Err))
	}

}

func (a *ResponsesWSSessionActor) handleProviderRecvFailed(event ResponsesWSEventProviderRecvFailed) {
	if !a.parallelProviderSource(event.UpstreamSessionGeneration, event.ChannelID) {
		return
	}
	a.writeProxyLocal(responsesWSProviderRecvFailureClientPayload(event))
	a.close("provider_recv_failed")
}

func (a *ResponsesWSSessionActor) handleProviderClosed(event ResponsesWSEventProviderClosed) {

	if !a.parallelProviderSource(event.UpstreamSessionGeneration, event.ChannelID) {
		return
	}
	if !a.closeProviderDownstream(event) && a.io.pump != nil {
		_ = a.io.pump.WriteClientFrame(responsesWSCloseMessageType, responsesWSProviderClosePayload(event.Code, event.Reason), ResponsesWSWriteProvider)
	}
	a.markDownstreamCloseSent()
	a.close("provider_closed")

}

func (a *ResponsesWSSessionActor) closeProviderDownstream(event ResponsesWSEventProviderClosed) bool {
	if a == nil || a.io.client == nil {
		return false
	}
	if a.closing.reducingCut {
		return true
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
	if !a.markProviderAPIErrorSeen(apiErr, source, responsesWSPayloadResponseID(payload)) {
		return
	}
	channel := a.providerPayloadChannel(channelID)
	processProviderAPIError(a.Context(), channel, apiErr, source)
}

func (a *ResponsesWSSessionActor) markProviderAPIErrorSeen(apiErr *types.OpenAIErrorWithStatusCode, source, responseID string) bool {
	if a == nil || apiErr == nil {
		return true
	}
	work := a.observation.byResponse(responseID)
	if work == nil {
		return true
	}
	attempt := work.attempt
	// Trade-off: dedupe only within the current turn attempt. This suppresses
	// repeated provider frames for the same failure without hiding the same
	// provider-side error if a later user turn fails independently.
	key := sha256.Sum256([]byte(providerAPIErrorDedupeKey(apiErr, source)))
	if _, ok := attempt.providerAPIErrorKeys[key]; ok {
		return false
	}
	if len(attempt.providerAPIErrorKeys) >= responsesWSObservationLimit {
		return false
	}
	if attempt.providerAPIErrorKeys == nil {
		attempt.providerAPIErrorKeys = make(map[[32]byte]struct{}, 1)
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

func (a *ResponsesWSSessionActor) providerPayloadChannel(_ int) *model.Channel {
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
	}
	// Provider-error accounting must never turn the actor into a synchronous DB
	// reader. The actual selected channel is recorded during provider work; if that
	// invariant is missing, metrics still record the error and auto-disable is
	// conservatively skipped.
	return nil
}

func responsesWSProviderRecvFailureClientPayload(event ResponsesWSEventProviderRecvFailed) []byte {
	if payload := responsesws.ClientPayloadFromError(event.Err); len(payload) > 0 {
		return payload
	}
	if responsesWSProviderLifecyclePolicyForEvent(upstreamEventFromProviderRecvFailed(event)).ProviderMalformedClientPayload {
		return responsesWSProviderProtocolErrorPayload("")
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

func (a *ResponsesWSSessionActor) clearPendingTurn(reason string) responsesWSPendingCleanup {

	cleanup := a.turns.ClearPending()
	if cleanup.attempt != nil && (cleanup.attempt.RolledBack || cleanup.attempt.QuotaFinalized) {
		a.observation.remove(cleanup.attempt.AttemptID)
	}
	return cleanup

}

func (a *ResponsesWSSessionActor) startQueuedResponseCreates() {

	if !a.allowNewWork() || a.upstream.session == nil {
		return
	}
	for a.allowNewWork() {
		queued, ok := a.turns.queue.Pop()
		if !ok {
			return
		}
		a.handleClientFrame(ResponsesWSEventClientFrame{Frame: responsesws.NewTextFrame(queued.payload), ReceivedAt: queued.receivedAt})
	}

}

func (a *ResponsesWSSessionActor) handleClientClosed(err error) {
	a.markClientClosed(err)
	if err != nil && !isResponsesWSExpectedClientDisconnectError(err) {
		a.logDebugf("responses websocket client close event: %T: %v", err, err)
	}
	a.close("client_closed")
}

func (a *ResponsesWSSessionActor) logClose(reason string) {

	a.logDebugf("responses websocket session closing: reason=%s work_candidates=%d retained_bytes=%d channel_id=%d", reason, len(a.observation.works), a.observation.bytes, a.upstream.channelID)

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

func (a *ResponsesWSSessionActor) isBusy() bool {
	return a.upstream.session == nil
}

func (a *ResponsesWSSessionActor) writeProxyLocal(payload []byte) {
	if a == nil || a.io.pump == nil || len(payload) == 0 || a.closing.reducingCut {
		return
	}
	if err := a.io.pump.WriteClientTypedFrame(responsesws.NewTextFrame(payload), ResponsesWSWriteProxyLocal); err != nil {
		a.markClientClosed(err)
		a.requestCloseIntent("client_write_failed")
	}
}

func (a *ResponsesWSSessionActor) writeProxyLocalForAttempt(attempt *ResponsesWSTurnAttempt, payload []byte, reason string) {
	if a == nil || len(payload) == 0 || a.closing.reducingCut {
		return
	}
	if err := a.emitProxyLocalForAttempt(attempt, payload, reason); err != nil {
		a.markClientClosed(err)
		a.requestCloseIntent("client_write_failed")
	}
}

func (a *ResponsesWSSessionActor) nextDownstreamSeq() uint64 {
	if a == nil {
		return 0
	}
	a.downstreamSeq++
	return a.downstreamSeq
}

func (a *ResponsesWSSessionActor) emitDownstream(attempt *ResponsesWSTurnAttempt, kind ResponsesDownstreamCommitKind, frame responsesws.Frame, mode ResponsesWSWriteMode, reason string) error {
	if a == nil || a.io.pump == nil {
		return nil
	}
	if a.closing.reducingCut {
		return nil
	}
	if attempt != nil && !attempt.DownstreamCommitted {
		attempt.MarkDownstreamCommitted(kind, reason, a.nextDownstreamSeq())
	}
	return a.io.pump.WriteClientTypedFrame(frame, mode)
}

func (a *ResponsesWSSessionActor) emitProxyLocalForAttempt(attempt *ResponsesWSTurnAttempt, payload []byte, reason string) error {
	if a == nil || a.io.pump == nil || len(payload) == 0 {
		return nil
	}
	return a.emitDownstream(attempt, DownstreamCommitProxyError, responsesws.NewTextFrame(payload), ResponsesWSWriteProxyLocal, reason)
}

func (a *ResponsesWSSessionActor) emitProviderFrameForAttempt(attempt *ResponsesWSTurnAttempt, frame responsesws.Frame, reason string) error {
	if a == nil || a.io.pump == nil || frame.IsZero() {
		return nil
	}
	if frame.Kind() == responsesws.FrameKindText {
		var credentials []string
		if source, ok := a.upstream.session.(providerresponse.CredentialSource); ok {
			credentials = source.ProviderCredentials()
		}
		if payload, changed := providerresponse.SanitizeErrorPayload(frame.Payload(), credentials...); changed {
			frame = responsesws.NewTextFrame(payload)
		}
	}
	return a.emitDownstream(attempt, DownstreamCommitProviderFrame, frame, ResponsesWSWriteProvider, reason)
}

func (a *ResponsesWSSessionActor) requestCloseIntent(reason string) {
	if a == nil || a.closing.closed.Load() {
		return
	}
	a.stopNewWork()
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
	if a == nil || a.closing.closed.Load() || a.closing.ingressClosed.Load() {
		return false
	}
	if a.tryPostEvent(event) {
		return true
	}
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

// 传输 ID 只标识发送调用；已经绑定的 Response 才是后续证据的 owner。

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

func upstreamEventFromProviderRecvFailed(event ResponsesWSEventProviderRecvFailed) responsesws.UpstreamEvent {
	return responsesws.UpstreamEvent{
		AttemptID:    event.AttemptID,
		DetailOrigin: event.DetailOrigin,
		DetailPhase:  event.DetailPhase,
		Err:          event.Err,
	}
}

func (a *ResponsesWSSessionActor) failClosed(reason string) {
	if a == nil || a.closing.closed.Load() {
		return
	}
	a.stopNewWork()
	a.writeProxyLocal(responsesWSErrorPayload(http.StatusBadGateway, "responses_ws_protocol_violation", reason))
	a.close(reason)
}

func (a *ResponsesWSSessionActor) close(reason string) {
	if a == nil || a.closing.closed.Load() {
		return
	}
	effectiveReason := strings.TrimSpace(reason)
	if effectiveReason == "" {
		effectiveReason = "session_closed"
	}
	if a.closing.reducingCut {
		return
	}
	drainCount := a.captureClosureCut()
	a.closing.reducingCut = true
	a.reduceClosureCut(drainCount)
	a.closing.reducingCut = false
	a.closeAfterClosureCut(effectiveReason)
}

func (a *ResponsesWSSessionActor) captureClosureCut() int {
	if a == nil {
		return 0
	}
	a.closing.postMu.Lock()
	defer a.closing.postMu.Unlock()
	a.closing.workStopped.Store(true)
	a.closing.ingressClosed.Store(true)
	return len(a.events)
}

func (a *ResponsesWSSessionActor) reduceClosureCut(count int) {
	if a == nil || count <= 0 {
		return
	}
	for i := 0; i < count; i++ {
		event := <-a.events
		a.handleEvent(event)
		a.releaseEventBytes(event)
		a.signalEventSpace()
	}
}

func (a *ResponsesWSSessionActor) closeAfterClosureCut(effectiveReason string) {

	if a == nil || a.closing.closed.Swap(true) {
		return
	}
	a.turns.queue.Clear()
	a.cancelSetup()
	a.releasePendingLease()
	defer a.releaseActiveLease()
	a.finishAllObservedWorks()
	a.logClose(effectiveReason)
	if a.upstream.session != nil && a.io.pump != nil {
		a.io.pump.AbortSession(a.upstream.session, effectiveReason)
	}
	if a.io.pump != nil && a.io.pump.writer != nil && !a.closing.downstreamCloseSent.Swap(true) {
		code := wsconn.CloseNormalClosure
		if effectiveReason == "provider_recv_failed" {
			code = wsconn.CloseInternalServerErr
		}
		a.io.pump.WriteCloseControl(int(code), responsesWSCloseReason(effectiveReason))
	}
	a.heldSteeringParents = nil
	a.heldSteeringParentBytes = 0
	a.state = responsesWSStateClosed
	a.finish()

}

func (a *ResponsesWSSessionActor) markDownstreamCloseSent() {
	if a != nil {
		a.closing.downstreamCloseSent.Store(true)
	}
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
	pendingBytes, apiErr := middleware.AcquireResponsesWSPendingByteLease(c)
	if apiErr != nil {
		common.AbortWithMessage(c, apiErr.StatusCode, apiErr.Message)
		return
	}
	defer pendingBytes.Release()

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
	firstFrameTimeout := config.ResponsesWSFirstFrameTimeout()
	firstCtx := c.Request.Context()
	cancelFirstFrame := func() {}
	if firstFrameTimeout > 0 {
		firstCtx, cancelFirstFrame = context.WithTimeout(firstCtx, firstFrameTimeout)
	}
	mt, raw, err := clientConn.ReadInitialWithReservation(firstCtx, pendingBytes.TryAcquire)
	cancelFirstFrame()
	firstFrameReceivedAt := time.Now()
	if err != nil {
		status := http.StatusBadRequest
		code := "invalid_event"
		closeKind := wsconn.CloseKindAbort
		closeCode := wsconn.ClosePolicyViolation
		if errors.Is(err, wsconn.ErrFirstFrameByteBudget) {
			status = http.StatusServiceUnavailable
			code = "responses_ws_pending_byte_capacity_exceeded"
			closeKind = wsconn.CloseKindBackpressure
			closeCode = wsconn.CloseTryAgainLater
		}
		_ = clientConn.WriteMessage(wsconn.TextMessage, responsesWSErrorPayload(status, code, responsesWSFirstFrameReadErrorMessage(err)))
		logger.LogWarn(c.Request.Context(), fmt.Sprintf("responses websocket first frame read failed: err=%v timeout=%s", err, firstFrameTimeout))
		clientConn.Close(wsconn.CloseInfo{Kind: closeKind, Code: closeCode, Reason: code, Err: err})
		return
	}
	if mt != wsconn.TextMessage {
		logger.LogWarn(c.Request.Context(), fmt.Sprintf("responses websocket first frame rejected: message_type=%d reason=text_only", mt))
		clientConn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindNormal, Code: wsconn.CloseUnsupportedData, Reason: "text_only"})
		return
	}
	frame, err := responsesws.ParseRawResponsesCreateFrameOwned(raw)
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
	ioPump := NewResponsesWSManagedPump(clientConn, actor)
	defer ioPump.Close()
	actor.SetPump(ioPump)
	actor.SetClientConn(clientConn)
	actor.Start()
	defer armResponsesWSMaxLifetime(actor)()
	clientPump := wsconn.Pump{
		Conn:    clientConn,
		Handle:  actor.onClientFrame,
		OnClose: actor.onClientConnClosed,
	}
	if !actor.PostReliable(ResponsesWSEventFirstTurnSetup{Frame: frame, PendingLease: pendingLease, PendingBytes: pendingBytes, ReceivedAt: firstFrameReceivedAt}) {
		pendingLease.Release()
		actor.requestCloseIntent("first_turn_setup_not_queued")
	} else {
		go clientPump.Run(c.Request.Context())
	}
	<-actor.Done()
	actor.waitStartedGoroutines()
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
