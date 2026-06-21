package responsesws

import (
	"encoding/json"
	"errors"
	"net/http"
	"one-api/types"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestBridgeEventResultLegalCombinations(t *testing.T) {
	frame := NewTextFrame([]byte(`{"type":"response.completed"}`))
	usage := &types.UsageEvent{TotalTokens: 1}
	baseErr := errors.New("bridge stream failed")

	cases := []struct {
		name   string
		result BridgeEventResult
	}{
		{
			name: "provider stream frame",
			result: BridgeEventResult{
				EmitFrame: &frame,
				Origin:    RecvDetailOriginProviderStream,
			},
		},
		{
			name: "provider stream frame with usage",
			result: BridgeEventResult{
				EmitFrame: &frame,
				Usage:     usage,
				Origin:    RecvDetailOriginProviderStream,
			},
		},
		{
			name: "provider stream usage only",
			result: BridgeEventResult{
				Usage:  usage,
				Origin: RecvDetailOriginProviderStream,
			},
		},
		{
			name: "stream opened",
			result: BridgeEventResult{
				Origin: RecvDetailOriginBridgeStreamOpened,
			},
		},
		{
			name: "open provider error",
			result: BridgeEventResult{
				Err:         baseErr,
				CloseStream: true,
				Origin:      RecvDetailOriginBridgeOpenProviderError,
			},
		},
		{
			name: "synthetic cancel closes stream",
			result: BridgeEventResult{
				EmitFrame:   &frame,
				CloseStream: true,
				Origin:      RecvDetailOriginSyntheticBridge,
			},
		},
		{
			name: "synthetic cancel without active stream",
			result: BridgeEventResult{
				EmitFrame: &frame,
				Origin:    RecvDetailOriginSyntheticBridge,
			},
		},
		{
			name: "stream eof",
			result: BridgeEventResult{
				CloseStream: true,
				Origin:      RecvDetailOriginBridgeStreamEOF,
			},
		},
		{
			name: "stream error",
			result: BridgeEventResult{
				Err:         baseErr,
				CloseStream: true,
				Origin:      RecvDetailOriginBridgeStreamError,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateBridgeEventResult(tc.result); err != nil {
				t.Fatalf("expected bridge event result to be legal, got %v", err)
			}
		})
	}
}

func TestBridgeEventResultRejectsIllegalCombinations(t *testing.T) {
	frame := NewTextFrame([]byte(`{"type":"response.completed"}`))
	usage := &types.UsageEvent{TotalTokens: 1}
	baseErr := errors.New("bridge stream failed")

	cases := []struct {
		name   string
		result BridgeEventResult
	}{
		{
			name: "empty provider stream event",
			result: BridgeEventResult{
				Origin: RecvDetailOriginProviderStream,
			},
		},
		{
			name: "provider stream with err",
			result: BridgeEventResult{
				Err:    baseErr,
				Origin: RecvDetailOriginProviderStream,
			},
		},
		{
			name: "provider stream closes stream",
			result: BridgeEventResult{
				EmitFrame:   &frame,
				CloseStream: true,
				Origin:      RecvDetailOriginProviderStream,
			},
		},
		{
			name: "stream opened with frame",
			result: BridgeEventResult{
				EmitFrame: &frame,
				Origin:    RecvDetailOriginBridgeStreamOpened,
			},
		},
		{
			name: "open provider error with frame",
			result: BridgeEventResult{
				EmitFrame:   &frame,
				Err:         baseErr,
				CloseStream: true,
				Origin:      RecvDetailOriginBridgeOpenProviderError,
			},
		},
		{
			name: "open provider error with usage",
			result: BridgeEventResult{
				Usage:       usage,
				Err:         baseErr,
				CloseStream: true,
				Origin:      RecvDetailOriginBridgeOpenProviderError,
			},
		},
		{
			name: "open provider error without close",
			result: BridgeEventResult{
				Err:    baseErr,
				Origin: RecvDetailOriginBridgeOpenProviderError,
			},
		},
		{
			name: "synthetic cancel without frame",
			result: BridgeEventResult{
				Origin: RecvDetailOriginSyntheticBridge,
			},
		},
		{
			name: "synthetic cancel with usage",
			result: BridgeEventResult{
				EmitFrame: &frame,
				Usage:     usage,
				Origin:    RecvDetailOriginSyntheticBridge,
			},
		},
		{
			name: "eof with err",
			result: BridgeEventResult{
				Err:         baseErr,
				CloseStream: true,
				Origin:      RecvDetailOriginBridgeStreamEOF,
			},
		},
		{
			name: "stream error without close",
			result: BridgeEventResult{
				Err:    baseErr,
				Origin: RecvDetailOriginBridgeStreamError,
			},
		},
		{
			name: "provider frame origin is not bridge event result",
			result: BridgeEventResult{
				EmitFrame: &frame,
				Origin:    RecvDetailOriginProviderFrame,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateBridgeEventResult(tc.result); !errors.Is(err, ErrInvalidBridgeEventResult) {
				t.Fatalf("expected invalid bridge event result error, got %v", err)
			}
		})
	}
}

func TestBridgeEventResultIsSeparateFromProviderFrameResult(t *testing.T) {
	frame := NewTextFrame([]byte(`{"type":"response.completed"}`))
	bridgeResult := BridgeEventResult{
		EmitFrame: &frame,
		Origin:    RecvDetailOriginProviderStream,
	}
	if err := ValidateBridgeEventResult(bridgeResult); err != nil {
		t.Fatalf("expected bridge provider stream result to be legal, got %v", err)
	}
	providerResult := ProviderFrameResult{
		EmitFrame: &frame,
		Origin:    RecvDetailOriginProviderStream,
	}
	if err := ValidateProviderFrameResult(providerResult); !errors.Is(err, ErrInvalidProviderFrameResult) {
		t.Fatalf("expected provider frame result to reject bridge stream origin, got %v", err)
	}
}

func TestMarkHTTPBridgeTransportErrorOnlyMarksRequesterTransportFailure(t *testing.T) {
	transportErr := &types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{Code: "http_request_failed", Type: "one_hub_error"},
		StatusCode:  http.StatusInternalServerError,
	}
	if got := MarkHTTPBridgeTransportError(transportErr); got != transportErr || !transportErr.LocalError {
		t.Fatalf("expected requester transport failure to be marked local, got %+v", got)
	}

	providerErr := &types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{Code: "http_request_failed", Type: "provider_error"},
		StatusCode:  http.StatusTooManyRequests,
	}
	if got := MarkHTTPBridgeTransportError(providerErr); got != providerErr || providerErr.LocalError {
		t.Fatalf("expected provider error to remain non-local, got %+v", got)
	}

	if got := MarkHTTPBridgeTransportError(nil); got != nil {
		t.Fatalf("expected nil error to remain nil, got %+v", got)
	}
}

func TestBridgeOpenProviderErrorPayloadIsBoundedAndSanitized(t *testing.T) {
	err := NewBridgeOpenProviderError(
		429,
		" rate_limit_exceeded\n",
		" throttling_error\t",
		"provider said no Authorization: Bearer secret upstream-url https://provider.example/v1/responses session abc "+strings.Repeat("tail ", 120),
	)
	payload := ClientPayloadFromError(err)
	if len(payload) == 0 {
		t.Fatal("expected client payload error")
	}

	var decoded struct {
		Type  string `json:"type"`
		Error struct {
			Status  int    `json:"status"`
			Code    string `json:"code"`
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if unmarshalErr := json.Unmarshal(payload, &decoded); unmarshalErr != nil {
		t.Fatalf("decode payload: %v", unmarshalErr)
	}
	if decoded.Type != "error" || decoded.Error.Status != 429 || decoded.Error.Code != "rate_limit_exceeded" || decoded.Error.Type != "throttling_error" {
		t.Fatalf("unexpected sanitized payload: %s", payload)
	}
	if len(decoded.Error.Message) > bridgeProviderErrorMessageLimit {
		t.Fatalf("expected bounded message, got len=%d", len(decoded.Error.Message))
	}
	if strings.ContainsAny(decoded.Error.Message, "\n\r\t") {
		t.Fatalf("expected control whitespace to be removed, got %q", decoded.Error.Message)
	}
	lowerMessage := strings.ToLower(decoded.Error.Message)
	for _, forbidden := range []string{"authorization", "bearer", "secret", "https://provider.example", "session abc"} {
		if strings.Contains(lowerMessage, forbidden) {
			t.Fatalf("expected sensitive provider text %q to be redacted, got %q", forbidden, decoded.Error.Message)
		}
	}
}

func TestSanitizeBridgeProviderErrorFieldTruncatesOnUTF8Boundary(t *testing.T) {
	value := strings.Repeat("a", 127) + "你" + "tail"
	got := sanitizeBridgeProviderErrorField(value)
	if !utf8.ValidString(got) {
		t.Fatalf("expected sanitized field to remain valid UTF-8, got %q", got)
	}
	if len(got) > 128 {
		t.Fatalf("expected sanitized field to be bounded to 128 bytes, got len=%d", len(got))
	}
	if got != strings.Repeat("a", 127) {
		t.Fatalf("expected truncation to back up to rune boundary, got %q", got)
	}
}

func TestBridgeOpenProviderErrorReusesTypedProviderErrorShape(t *testing.T) {
	err := NewBridgeOpenProviderErrorFromOpenAIError(&types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{
			Code:    12345,
			Type:    "provider_rate_limit_error",
			Message: "provider Authorization Bearer secret response_body raw body",
		},
		StatusCode: 429,
	})
	payload := ClientPayloadFromError(err)
	if len(payload) == 0 {
		t.Fatal("expected client payload error")
	}
	var decoded struct {
		Error struct {
			Status  int    `json:"status"`
			Code    string `json:"code"`
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if unmarshalErr := json.Unmarshal(payload, &decoded); unmarshalErr != nil {
		t.Fatalf("decode payload: %v", unmarshalErr)
	}
	if decoded.Error.Status != 429 || decoded.Error.Code != "12345" || decoded.Error.Type != "provider_rate_limit_error" {
		t.Fatalf("expected typed provider error fields to be reused, got %s", payload)
	}
	lowerMessage := strings.ToLower(decoded.Error.Message)
	for _, forbidden := range []string{"authorization", "bearer", "secret", "response_body", "raw body"} {
		if strings.Contains(lowerMessage, forbidden) {
			t.Fatalf("expected typed provider error message to be sanitized, got %q", decoded.Error.Message)
		}
	}
}

func TestBridgeOpenProviderErrorRedactsCommonCredentialShapes(t *testing.T) {
	err := NewBridgeOpenProviderError(
		400,
		"invalid_request",
		"provider_error",
		"bad key sk-testSECRET123 sk-proj-projectSECRET123 api_key=abc123 token=tok456 access_token=acc789 jwt eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.signatureSECRET",
	)
	payload := ClientPayloadFromError(err)
	if len(payload) == 0 {
		t.Fatal("expected client payload error")
	}
	var decoded struct {
		Error struct {
			Code    string `json:"code"`
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if unmarshalErr := json.Unmarshal(payload, &decoded); unmarshalErr != nil {
		t.Fatalf("decode payload: %v", unmarshalErr)
	}
	if decoded.Error.Code != "invalid_request" || decoded.Error.Type != "provider_error" {
		t.Fatalf("expected provider code/type to remain, got %s", payload)
	}
	for _, forbidden := range []string{"sk-testSECRET123", "sk-proj-projectSECRET123", "abc123", "tok456", "acc789", "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9"} {
		if strings.Contains(decoded.Error.Message, forbidden) {
			t.Fatalf("expected credential fragment %q to be redacted, got %q", forbidden, decoded.Error.Message)
		}
	}
}
