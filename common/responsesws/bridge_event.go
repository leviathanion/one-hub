package responsesws

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"one-api/types"
)

const bridgeProviderErrorMessageLimit = 512

var ErrInvalidBridgeEventResult = errors.New("invalid responses websocket bridge event result")

// BridgeEventResult is the HTTP bridge's typed stream event output. It carries
// provider/proxy evidence to the relay actor without owning accounting.
type BridgeEventResult struct {
	EmitFrame   *Frame
	Usage       *types.UsageEvent
	Err         error
	CloseStream bool
	Origin      RecvDetailOrigin
}

func ValidateBridgeEventResult(result BridgeEventResult) error {
	switch result.Origin {
	case RecvDetailOriginProviderStream:
		if result.Err != nil || result.CloseStream || (result.EmitFrame == nil && result.Usage == nil) {
			return ErrInvalidBridgeEventResult
		}
		return nil
	case RecvDetailOriginBridgeStreamOpened:
		if result.EmitFrame != nil || result.Usage != nil || result.Err != nil || result.CloseStream {
			return ErrInvalidBridgeEventResult
		}
		return nil
	case RecvDetailOriginBridgeOpenProviderError:
		if result.EmitFrame != nil || result.Usage != nil || result.Err == nil || !result.CloseStream {
			return ErrInvalidBridgeEventResult
		}
		return nil
	case RecvDetailOriginSyntheticBridge:
		if result.EmitFrame == nil || result.Usage != nil || result.Err != nil {
			return ErrInvalidBridgeEventResult
		}
		return nil
	case RecvDetailOriginBridgeStreamEOF:
		if result.EmitFrame != nil || result.Usage != nil || result.Err != nil || !result.CloseStream {
			return ErrInvalidBridgeEventResult
		}
		return nil
	case RecvDetailOriginBridgeStreamError:
		if result.EmitFrame != nil || result.Usage != nil || result.Err == nil || !result.CloseStream {
			return ErrInvalidBridgeEventResult
		}
		return nil
	case RecvDetailOriginAdapterPanic:
		if result.EmitFrame != nil || result.Usage != nil || result.Err == nil || !result.CloseStream {
			return ErrInvalidBridgeEventResult
		}
		return nil
	default:
		return ErrInvalidBridgeEventResult
	}
}

// BridgeOpenProviderError describes a provider HTTP rejection before a bridge
// stream starts.
type BridgeOpenProviderError struct {
	StatusCode int
	Code       string
	Type       string
	Message    string
}

func (e *BridgeOpenProviderError) Error() string {
	if e == nil {
		return ""
	}
	parts := []string{"responses websocket bridge provider open error"}
	if e.StatusCode > 0 {
		parts = append(parts, "status "+strconv.Itoa(e.StatusCode))
	}
	if e.Code != "" {
		parts = append(parts, "code "+e.Code)
	}
	if e.Type != "" {
		parts = append(parts, "type "+e.Type)
	}
	return strings.Join(parts, ": ")
}

func NewBridgeOpenProviderError(statusCode int, code, errorType, message string) error {
	return NewBridgeOpenProviderErrorFromOpenAIError(&types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{
			Code:    code,
			Type:    errorType,
			Message: message,
		},
		StatusCode: statusCode,
	})
}

func NewBridgeOpenLocalErrorFromOpenAIError(errWithStatus *types.OpenAIErrorWithStatusCode) error {
	if errWithStatus == nil {
		return nil
	}
	statusCode := errWithStatus.StatusCode
	if statusCode <= 0 {
		statusCode = http.StatusBadGateway
	}
	code := bridgeProviderErrorCodeString(errWithStatus.OpenAIError.Code)
	if strings.TrimSpace(code) == "" {
		code = "ws_request_failed"
	}
	errorType := strings.TrimSpace(errWithStatus.OpenAIError.Type)
	if errorType == "" {
		errorType = "one_hub_error"
	}
	payload, err := json.Marshal(struct {
		Type   string `json:"type"`
		Status int    `json:"status"`
		Error  struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{
		Type:   "error",
		Status: statusCode,
		Error: struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		}{
			Type:    sanitizeBridgeProviderErrorField(errorType),
			Code:    sanitizeBridgeProviderErrorField(code),
			Message: bridgeOpenLocalClientMessage(statusCode, code),
		},
	})
	if err != nil {
		return errWithStatus
	}
	return NewClientPayloadError(errWithStatus, payload)
}

func IsBridgeOpenLocalError(err error) bool {
	var errWithStatus *types.OpenAIErrorWithStatusCode
	return errors.As(err, &errWithStatus) && errWithStatus != nil && errWithStatus.LocalError
}

func bridgeOpenLocalClientMessage(statusCode int, code string) string {
	code = strings.TrimSpace(code)
	if statusCode == http.StatusGatewayTimeout {
		return "responses websocket bridge stream opening timed out"
	}
	switch code {
	case "ws_request_failed":
		return "responses websocket request failed"
	default:
		return "responses websocket request failed"
	}
}

func NewBridgeOpenProviderErrorFromOpenAIError(errWithStatus *types.OpenAIErrorWithStatusCode) error {
	var statusCode int
	var code string
	var errorType string
	var message string
	if errWithStatus != nil {
		statusCode = errWithStatus.StatusCode
		code = bridgeProviderErrorCodeString(errWithStatus.OpenAIError.Code)
		errorType = errWithStatus.OpenAIError.Type
		message = errWithStatus.OpenAIError.Message
	}
	providerErr := &BridgeOpenProviderError{
		StatusCode: statusCode,
		Code:       sanitizeBridgeProviderErrorField(code),
		Type:       sanitizeBridgeProviderErrorField(errorType),
		Message:    sanitizeBridgeProviderErrorMessage(message),
	}
	payload, err := json.Marshal(struct {
		Type  string `json:"type"`
		Error struct {
			Status  int    `json:"status,omitempty"`
			Code    string `json:"code,omitempty"`
			Type    string `json:"type,omitempty"`
			Message string `json:"message,omitempty"`
		} `json:"error"`
	}{
		Type: "error",
		Error: struct {
			Status  int    `json:"status,omitempty"`
			Code    string `json:"code,omitempty"`
			Type    string `json:"type,omitempty"`
			Message string `json:"message,omitempty"`
		}{
			Status:  providerErr.StatusCode,
			Code:    providerErr.Code,
			Type:    providerErr.Type,
			Message: providerErr.Message,
		},
	})
	if err != nil {
		return providerErr
	}
	return NewClientPayloadError(providerErr, payload)
}

func BridgeOpenProviderErrorRecoverable(err error) bool {
	var providerErr *BridgeOpenProviderError
	if !errors.As(err, &providerErr) || providerErr == nil {
		return false
	}
	switch providerErr.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return false
	default:
		return true
	}
}

// MarkHTTPBridgeTransportError restores bridge-local transport semantics for
// SendRequestRaw failures without changing generic HTTP relay retry behavior.
func MarkHTTPBridgeTransportError(errWithStatus *types.OpenAIErrorWithStatusCode) *types.OpenAIErrorWithStatusCode {
	if errWithStatus == nil {
		return nil
	}
	if errWithStatus.StatusCode == http.StatusInternalServerError &&
		errWithStatus.OpenAIError.Type == "one_hub_error" &&
		bridgeProviderErrorCodeString(errWithStatus.OpenAIError.Code) == "http_request_failed" {
		errWithStatus.LocalError = true
	}
	return errWithStatus
}

func bridgeProviderErrorCodeString(code any) string {
	switch value := code.(type) {
	case nil:
		return ""
	case string:
		return value
	default:
		return strings.TrimSpace(fmt.Sprint(value))
	}
}

func sanitizeBridgeProviderErrorField(value string) string {
	value = sanitizeBridgeProviderErrorMessage(value)
	return truncateBridgeProviderErrorUTF8(value, 128)
}

func sanitizeBridgeProviderErrorMessage(message string) string {
	message = strings.TrimSpace(message)
	if message == "" {
		return ""
	}
	message = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, message)
	message = strings.Join(strings.Fields(message), " ")
	message = RedactSensitiveText(message)
	if len(message) <= bridgeProviderErrorMessageLimit {
		return message
	}
	return truncateBridgeProviderErrorUTF8(message, bridgeProviderErrorMessageLimit)
}

func truncateBridgeProviderErrorUTF8(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	truncated := value[:limit]
	for !utf8.ValidString(truncated) && len(truncated) > 0 {
		truncated = truncated[:len(truncated)-1]
	}
	return truncated
}
