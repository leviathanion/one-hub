package responsesws

import "errors"

var ErrInvalidResponsesWSTransportSendResult = errors.New("invalid responses websocket send result")

type ResponsesWSTransportSendStatus string
type ResponsesWSTransportSendReason string

const (
	ResponsesWSTransportSendNotAttempted         ResponsesWSTransportSendStatus = "not_attempted"
	ResponsesWSTransportSendAttempted            ResponsesWSTransportSendStatus = "attempted"
	ResponsesWSTransportSendRejectedBeforeStream ResponsesWSTransportSendStatus = "rejected_before_stream"
	ResponsesWSTransportSendAmbiguous            ResponsesWSTransportSendStatus = "ambiguous"
)

const (
	ResponsesWSTransportSendReasonNoActiveBridgeCancel ResponsesWSTransportSendReason = "no_active_bridge_cancel"
	ResponsesWSTransportSendReasonStaleBridgeCancel    ResponsesWSTransportSendReason = "stale_bridge_cancel"
)

type ResponsesWSTransportSendResult struct {
	Status ResponsesWSTransportSendStatus
	Err    error
	// Reason is diagnostic-only. Response.create accounting must use Status plus provider evidence.
	Reason ResponsesWSTransportSendReason
}

func ValidateResponsesWSTransportSendResult(result ResponsesWSTransportSendResult) error {
	switch result.Status {
	case ResponsesWSTransportSendAttempted, ResponsesWSTransportSendRejectedBeforeStream:
		if result.Err != nil || result.Reason != "" {
			return ErrInvalidResponsesWSTransportSendResult
		}
		return nil
	case ResponsesWSTransportSendNotAttempted:
		if result.Err == nil && result.Reason == "" {
			return ErrInvalidResponsesWSTransportSendResult
		}
		return nil
	case ResponsesWSTransportSendAmbiguous:
		if result.Err == nil || result.Reason != "" {
			return ErrInvalidResponsesWSTransportSendResult
		}
		return nil
	default:
		return ErrInvalidResponsesWSTransportSendResult
	}
}
