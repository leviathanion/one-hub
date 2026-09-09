package responses

import "strings"

// StreamErrorEvent is the public Responses streaming error envelope.
type StreamErrorEvent struct {
	Type           string  `json:"type"`
	Code           string  `json:"code"`
	Message        string  `json:"message"`
	Param          *string `json:"param"`
	SequenceNumber int64   `json:"sequence_number"`
}

func NewStreamErrorEvent(sequenceNumber int64, code, message string) StreamErrorEvent {
	return StreamErrorEvent{
		Type:           "error",
		Code:           strings.TrimSpace(code),
		Message:        strings.TrimSpace(message),
		SequenceNumber: sequenceNumber,
	}
}
