package relay

import (
	"errors"
	"one-api/common/responsesws"
	"one-api/common/wsconn"
	"one-api/metrics"
	"one-api/middleware"
	"one-api/types"
	"time"
)

type ResponsesWSSendPurpose string

const (
	ResponsesWSSendPurposeResponseCreate ResponsesWSSendPurpose = "response_create"
	ResponsesWSSendPurposeResponseCancel ResponsesWSSendPurpose = "response_cancel"
	ResponsesWSSendPurposeControl        ResponsesWSSendPurpose = "control"
	ResponsesWSSendPurposePingPong       ResponsesWSSendPurpose = "ping_pong"
)

const (
	responsesWSEventQueueSize           = 128
	responsesWSSendQueueSize            = 64
	responsesWSPendingProviderEventsMax = 32
	responsesWSRecentResponseIDLimit    = 16
	responsesWSBusyRejectLimit          = 16
	responsesWSBusyRejectWindow         = 10 * time.Second
	responsesWSConnectionSessionIDKey   = "responses_ws_connection_session_id"
)

const (
	responsesWSTextMessageType   = int(wsconn.TextMessage)
	responsesWSBinaryMessageType = int(wsconn.BinaryMessage)
	// Close is an actor-internal downstream sentinel. wsconn intentionally
	// exposes only data-frame message types, so provider close forwarding stays
	// local to ResponsesWS instead of widening the transport API.
	responsesWSCloseMessageType = 8
)

const (
	responsesWSActiveTurnTimeoutReason                    = "responses_ws_active_turn_timeout"
	responsesWSBridgeProviderRejectionWaitTimeoutReason   = "responses_ws_bridge_provider_rejection_wait_timeout"
	responsesWSBridgeLocalOpenErrorWaitTimeoutReason      = "responses_ws_bridge_local_open_error_wait_timeout"
	responsesWSBridgeProviderRejectionFallbackErrorCode   = "provider_rejected_before_stream"
	responsesWSBridgeProviderRejectionFallbackErrorReason = "upstream rejected response before stream"
)

var (
	errResponsesWSSendQueueFull           = errors.New("responses websocket upstream send queue is full")
	errResponsesWSClientFrameBackpressure = errors.New("responses websocket client frame backpressure")
	errResponsesWSEventPostTimeout        = errors.New("responses websocket actor event post timed out")
)

const defaultResponsesWSReliablePostTimeout = 30 * time.Second

var (
	responsesWSBridgeProviderRejectionWaitTimeout = 200 * time.Millisecond
	responsesWSBridgeLocalOpenErrorWaitTimeout    = 200 * time.Millisecond
	recordUsageObservedUnbilled                   = metrics.RecordUsageObservedUnbilled
	recordResponsesWSEventPostTimeout             = metrics.RecordResponsesWSEventPostTimeout
	recordResponsesWSSettlementConflict           = metrics.RecordResponsesWSSettlementConflict
	recordResponsesWSAttemptReplayDecision        = metrics.RecordResponsesWSAttemptReplayDecision
	recordResponsesWSAttemptReplayExecuted        = metrics.RecordResponsesWSAttemptReplayExecuted
	recordResponsesWSAttemptReplayBlocked         = metrics.RecordResponsesWSAttemptReplayBlocked
)

type ResponsesWSEvent interface{ responsesWSEvent() }

type ResponsesWSEventClientFrame struct {
	Frame      responsesws.Frame
	ReceivedAt time.Time
}

func (ResponsesWSEventClientFrame) responsesWSEvent() {}

type ResponsesWSEventSendResult struct {
	AttemptID                 string
	ResponseID                string
	UpstreamSessionGeneration string
	SelectedChannelID         int
	Purpose                   ResponsesWSSendPurpose
	TransportResult           responsesws.ResponsesWSTransportSendResult
}

func (ResponsesWSEventSendResult) responsesWSEvent() {}

type ResponsesWSEventTransportContractViolation struct {
	AttemptID                 string
	ResponseID                string
	UpstreamSessionGeneration string
	SelectedChannelID         int
	Purpose                   ResponsesWSSendPurpose
	TransportResult           responsesws.ResponsesWSTransportSendResult
	Err                       error
}

func (ResponsesWSEventTransportContractViolation) responsesWSEvent() {}

type ResponsesWSProviderDownstreamKind int

const (
	ProviderDownstreamFrame ResponsesWSProviderDownstreamKind = iota
	ProviderDownstreamClose
)

type ResponsesWSEventBridgeOpenProviderError struct {
	UpstreamSessionGeneration string
	ChannelID                 int
	AttemptID                 string
	DetailPhase               responsesws.RecvDetailPhase
	Payload                   []byte
	ProviderAPIError          *types.OpenAIErrorWithStatusCode
	Recoverable               bool
}

func (ResponsesWSEventBridgeOpenProviderError) responsesWSEvent() {}

type ResponsesWSEventBridgeOpenLocalError struct {
	UpstreamSessionGeneration string
	ChannelID                 int
	AttemptID                 string
	DetailPhase               responsesws.RecvDetailPhase
	Payload                   []byte
	Recoverable               bool
}

func (ResponsesWSEventBridgeOpenLocalError) responsesWSEvent() {}

type ResponsesWSEventProxyLocalError struct {
	UpstreamSessionGeneration string
	ChannelID                 int
	AttemptID                 string
	DetailOrigin              responsesws.RecvDetailOrigin
	DetailPhase               responsesws.RecvDetailPhase
	Payload                   []byte
	ProviderAPIError          *types.OpenAIErrorWithStatusCode
	Recoverable               bool
}

func (ResponsesWSEventProxyLocalError) responsesWSEvent() {}

type ResponsesWSEventProviderDownstream struct {
	UpstreamSessionGeneration string
	ChannelID                 int
	AttemptID                 string
	ResponseID                string
	Kind                      ResponsesWSProviderDownstreamKind
	Frame                     *responsesws.Frame
	CloseCode                 int
	CloseReason               string
	Usage                     *types.UsageEvent
	Err                       error
	DetailOrigin              responsesws.RecvDetailOrigin
	DetailPhase               responsesws.RecvDetailPhase
	ReceivedAt                time.Time
}

func (ResponsesWSEventProviderDownstream) responsesWSEvent() {}

type ResponsesWSEventProviderUsageObserved struct {
	UpstreamSessionGeneration string
	ChannelID                 int
	AttemptID                 string
	ResponseID                string
	Usage                     *types.UsageEvent
	DetailOrigin              responsesws.RecvDetailOrigin
	DetailPhase               responsesws.RecvDetailPhase
	ReceivedAt                time.Time
}

func (ResponsesWSEventProviderUsageObserved) responsesWSEvent() {}

type ResponsesWSEventProviderBusinessError struct {
	UpstreamSessionGeneration string
	ChannelID                 int
	AttemptID                 string
	Err                       error
	DetailOrigin              responsesws.RecvDetailOrigin
	DetailPhase               responsesws.RecvDetailPhase
}

func (ResponsesWSEventProviderBusinessError) responsesWSEvent() {}

type ResponsesWSEventProviderRecvFailed struct {
	UpstreamSessionGeneration string
	ChannelID                 int
	AttemptID                 string
	Err                       error
	DetailOrigin              responsesws.RecvDetailOrigin
	DetailPhase               responsesws.RecvDetailPhase
	ReceivedAt                time.Time
}

func (ResponsesWSEventProviderRecvFailed) responsesWSEvent() {}

type ResponsesWSEventProviderClosed struct {
	UpstreamSessionGeneration string
	ChannelID                 int
	AttemptID                 string
	Code                      int
	Reason                    string
	Err                       error
	DetailOrigin              responsesws.RecvDetailOrigin
	DetailPhase               responsesws.RecvDetailPhase
	ReceivedAt                time.Time
}

func (ResponsesWSEventProviderClosed) responsesWSEvent() {}

type ResponsesWSEventClientClosed struct{ Err error }

func (ResponsesWSEventClientClosed) responsesWSEvent() {}

type ResponsesWSEventFirstTurnSetup struct {
	Frame        *responsesws.RawResponsesCreateFrame
	PendingLease middleware.ResponsesWSLease
	ReceivedAt   time.Time
}

func (ResponsesWSEventFirstTurnSetup) responsesWSEvent() {}

type ResponsesWSEventFirstTurnOpenResult struct {
	OpeningID  string
	Snapshot   *ResponsesWSRequestSnapshot
	OpenResult *responsesWSOpenResult
	Err        *types.OpenAIErrorWithStatusCode
	Adopted    chan bool
}

func (ResponsesWSEventFirstTurnOpenResult) responsesWSEvent() {}

type ResponsesWSEventTimeout struct {
	Reason                    string
	UpstreamSessionGeneration string
	ChannelID                 int
	AttemptID                 string
	TimeoutGeneration         int64
}

func (ResponsesWSEventTimeout) responsesWSEvent() {}

type ResponsesWSEventCloseIntent struct {
	Reason string
}

func (ResponsesWSEventCloseIntent) responsesWSEvent() {}

func responsesWSEventTypeLabel(event ResponsesWSEvent) string {
	switch event.(type) {
	case ResponsesWSEventClientFrame:
		return "client_frame"
	case ResponsesWSEventSendResult:
		return "send_result"
	case ResponsesWSEventTransportContractViolation:
		return "transport_contract_violation"
	case ResponsesWSEventBridgeOpenProviderError:
		return "bridge_open_provider_error"
	case ResponsesWSEventBridgeOpenLocalError:
		return "bridge_open_local_error"
	case ResponsesWSEventProxyLocalError:
		return "proxy_local_error"
	case ResponsesWSEventProviderDownstream:
		return "provider_downstream"
	case ResponsesWSEventProviderUsageObserved:
		return "provider_usage_observed"
	case ResponsesWSEventProviderBusinessError:
		return "provider_business_error"
	case ResponsesWSEventProviderRecvFailed:
		return "provider_recv_failed"
	case ResponsesWSEventProviderClosed:
		return "provider_closed"
	case ResponsesWSEventClientClosed:
		return "client_closed"
	case ResponsesWSEventFirstTurnSetup:
		return "first_turn_setup"
	case ResponsesWSEventFirstTurnOpenResult:
		return "first_turn_open_result"
	case ResponsesWSEventTimeout:
		return "timeout"
	case ResponsesWSEventCloseIntent:
		return "close_intent"
	default:
		return "unknown"
	}
}
