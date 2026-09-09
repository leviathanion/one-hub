package responsesws

import "errors"

var ErrInvalidResponsesWSTransportSendResult = errors.New("invalid responses websocket send result")

type ResponsesWSTransportSendStatus string
type ResponsesWSTransportSendReason string

const (
	ResponsesWSTransportSendNotAttempted ResponsesWSTransportSendStatus = "not_attempted"
	ResponsesWSTransportSendAttempted    ResponsesWSTransportSendStatus = "attempted"
	ResponsesWSTransportSendAmbiguous    ResponsesWSTransportSendStatus = "ambiguous"
)

type ResponsesWSTransportSendResult struct {
	Status ResponsesWSTransportSendStatus
	Err    error
	// Reason is diagnostic-only. Response.create accounting must use Status plus provider evidence.
	Reason ResponsesWSTransportSendReason
}

func ValidateResponsesWSTransportSendResult(result ResponsesWSTransportSendResult) error {
	switch result.Status {
	case ResponsesWSTransportSendAttempted:
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
