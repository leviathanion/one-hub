package relay

import (
	"errors"
	"one-api/common/responsesws"
	"one-api/common/wsconn"
	"one-api/metrics"
	"one-api/middleware"
	runtimesession "one-api/runtime/session"
	"one-api/types"
	"time"
)

type ResponsesWSSendOutcome int

const (
	SendOutcomeUnknown ResponsesWSSendOutcome = iota
	SendOutcomeNotSent
	SendOutcomeLocalWriteOK
	SendOutcomeAmbiguous
)

type BillingEvidence int

const (
	BillingEvidenceNone BillingEvidence = iota
	BillingEvidenceProviderUsageSeen
	BillingEvidenceProviderAcceptedTurnEvidence
)

const (
	responsesWSEventQueueSize               = 128
	responsesWSSendQueueSize                = 64
	responsesWSPendingProviderEventsMax     = 32
	responsesWSBusyRejectLimit              = 16
	responsesWSBusyRejectWindow             = 10 * time.Second
	responsesWSPreviousResponseIDContextKey = "responses_ws_previous_response_id"
	responsesWSConnectionSessionIDKey       = "responses_ws_connection_session_id"
)

const (
	responsesWSTextMessageType   = int(wsconn.TextMessage)
	responsesWSBinaryMessageType = int(wsconn.BinaryMessage)
	// Close is an actor-internal downstream sentinel. wsconn intentionally
	// exposes only data-frame message types, so provider close forwarding stays
	// local to ResponsesWS instead of widening the transport API.
	responsesWSCloseMessageType = 8
)

var (
	errResponsesWSSendQueueFull           = errors.New("responses websocket upstream send queue is full")
	errResponsesWSClientFrameBackpressure = errors.New("responses websocket client frame backpressure")
)

var recordUsageObservedUnbilled = metrics.RecordUsageObservedUnbilled

type ResponsesWSEvent interface{ responsesWSEvent() }

type ResponsesWSEventClientFrame struct {
	MessageType int
	Payload     []byte
	ReceivedAt  time.Time
}

func (ResponsesWSEventClientFrame) responsesWSEvent() {}

type ResponsesWSEventSendResult struct {
	AttemptID         string
	SelectedChannelID int
	Outcome           ResponsesWSSendOutcome
	Err               error
}

func (ResponsesWSEventSendResult) responsesWSEvent() {}

type ResponsesWSProviderDownstreamKind int

const (
	ProviderDownstreamFrame ResponsesWSProviderDownstreamKind = iota
)

type ResponsesWSEventProxyLocalError struct {
	UpstreamSessionGeneration string
	ChannelID                 int
	Payload                   []byte
	Recoverable               bool
}

func (ResponsesWSEventProxyLocalError) responsesWSEvent() {}

type ResponsesWSEventProviderDownstream struct {
	UpstreamSessionGeneration string
	ChannelID                 int
	Kind                      ResponsesWSProviderDownstreamKind
	MessageType               int
	Payload                   []byte
	Usage                     *types.UsageEvent
	Err                       error
	Origin                    runtimesession.RealtimePayloadOrigin
	ReceivedAt                time.Time
}

func (ResponsesWSEventProviderDownstream) responsesWSEvent() {}

type ResponsesWSEventProviderUsageObserved struct {
	UpstreamSessionGeneration string
	ChannelID                 int
	Usage                     *types.UsageEvent
	Origin                    runtimesession.RealtimePayloadOrigin
	ReceivedAt                time.Time
}

func (ResponsesWSEventProviderUsageObserved) responsesWSEvent() {}

type ResponsesWSEventProviderBusinessError struct {
	UpstreamSessionGeneration string
	ChannelID                 int
	Err                       error
}

func (ResponsesWSEventProviderBusinessError) responsesWSEvent() {}

type ResponsesWSEventProviderRecvFailed struct {
	UpstreamSessionGeneration string
	ChannelID                 int
	Err                       error
}

func (ResponsesWSEventProviderRecvFailed) responsesWSEvent() {}

type ResponsesWSEventProviderClosed struct {
	UpstreamSessionGeneration string
	ChannelID                 int
	Code                      int
	Reason                    string
	Err                       error
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
}

func (ResponsesWSEventTimeout) responsesWSEvent() {}

type ResponsesWSEventCloseIntent struct {
	Reason string
}

func (ResponsesWSEventCloseIntent) responsesWSEvent() {}
