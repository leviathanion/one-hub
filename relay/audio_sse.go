package relay

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"one-api/common"
	"one-api/common/providerresponse"
	"one-api/common/requestctx"
	"one-api/common/requester"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

const nativeSSEMaxEventBytes = 16 << 20

type nativeSSEHandler struct {
	credentials          []string
	framer               *requester.SSEEventFramer
	errorSeen            bool
	providerError        *types.OpenAIErrorWithStatusCode
	observeProviderEvent func([]byte)
	afterFrame           func([]byte)
}

func newNativeSSEHandler(observers ...func([]byte)) *nativeSSEHandler {
	handler := &nativeSSEHandler{framer: requester.NewSSEEventFramer(nativeSSEMaxEventBytes)}
	if len(observers) > 0 {
		handler.observeProviderEvent = observers[0]
	}
	return handler
}

func (h *nativeSSEHandler) Handle(rawLine *[]byte, emitter requester.StreamEmitter[string]) {
	if h == nil || rawLine == nil {
		return
	}
	event, complete, err := h.framer.PushLine(*rawLine)
	*rawLine = nil
	if err != nil {
		*rawLine = requester.StreamClosed
		emitter.SendError(err)
		return
	}
	if !complete {
		return
	}
	if h.afterFrame != nil {
		h.afterFrame(append([]byte(nil), event...))
	}

	_, _, payload := audioSSEFacts(event)
	errorEnvelope := sseEventContainsError(event)
	if !errorEnvelope && h.observeProviderEvent != nil {
		h.observeProviderEvent(payload)
	}
	safeEvent := redactProviderSSEEvent(string(event), h.credentials...)
	if !emitter.SendData(safeEvent) {
		return
	}
	if errorEnvelope {
		h.errorSeen = true
		h.providerError = providerAPIErrorFromAudioSSE(payload)
		if h.providerError == nil {
			h.providerError = common.StringErrorWrapper("provider stream failed", "upstream_error", http.StatusBadGateway)
		}
		h.providerError.UpstreamAccepted = true
	}
}

func audioSSEFacts(event []byte) (eventName, payloadType string, payload []byte) {
	dataLines := make([]string, 0, 1)
	for rest := string(event); rest != ""; {
		var line string
		line, rest = requester.SplitSSELine(rest)
		line = requester.SSELineContent(line)
		switch {
		case strings.HasPrefix(line, "event:"):
			eventName = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(line, "event:")))
		case strings.HasPrefix(line, "data:"):
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	payload = []byte(strings.Join(dataLines, "\n"))
	var envelope struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(payload, &envelope) == nil {
		payloadType = strings.ToLower(strings.TrimSpace(envelope.Type))
	}
	return eventName, payloadType, payload
}

func sseEventContainsError(event []byte) bool {
	eventName, _, payload := audioSSEFacts(event)
	return providerresponse.SSEPayloadContainsError(eventName, payload)
}

func providerAPIErrorFromAudioSSE(payload []byte) *types.OpenAIErrorWithStatusCode {
	var envelope struct {
		Error   *types.OpenAIError `json:"error"`
		Code    any                `json:"code"`
		Type    string             `json:"type"`
		Message string             `json:"message"`
		Param   any                `json:"param"`
	}
	if json.Unmarshal(payload, &envelope) != nil {
		return nil
	}
	apiErr := envelope.Error
	if apiErr == nil && (envelope.Message != "" || envelope.Code != nil) {
		apiErr = &types.OpenAIError{Message: envelope.Message, Type: envelope.Type, Code: envelope.Code}
		if envelope.Param != nil {
			apiErr.Param = strings.TrimSpace(strings.TrimSpace(toString(envelope.Param)))
		}
	}
	if apiErr == nil {
		return nil
	}
	return &types.OpenAIErrorWithStatusCode{OpenAIError: *apiErr, StatusCode: http.StatusBadGateway, UpstreamAccepted: true}
}

func toString(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	return string(encoded)
}

func responseNativeSSEClient(c *gin.Context, response *http.Response, operation providerresponse.Operation, observers ...func([]byte)) (time.Time, *types.OpenAIErrorWithStatusCode) {
	if response == nil || response.Body == nil {
		return time.Time{}, common.StringErrorWrapperLocal("provider stream response is missing", "invalid_provider_response", http.StatusBadGateway)
	}
	c.Set(requestctx.ProviderResponseHeadersContextKey, providerresponse.Filter(response.Header, providerresponse.Policy{
		Operation:      operation,
		DataPath:       providerresponse.DataPathSameDialect,
		BodyUnmodified: false,
	}))
	c.Set(requestctx.ProviderResponseStatusContextKey, response.StatusCode)
	handler := newNativeSSEHandler(observers...)
	handler.credentials = providerresponse.RequestCredentials(response.Request)
	stream, apiErr := requester.RequestNoTrimStreamWithEmitterOptions[string](nil, response, handler.Handle, requester.StreamReadOptions{
		MaxLineBytes: nativeSSEMaxEventBytes,
		SSELines:     true,
	})
	if apiErr != nil {
		return time.Time{}, apiErr
	}
	firstResponse, streamErr := responseGeneralStreamClientWithObserverResult(c, stream, nil, nil, nil, false)
	if streamErr == nil {
		if handler.errorSeen {
			c.Set(streamErrorAlreadyRenderedContextKey, true)
		}
		return firstResponse, handler.providerError
	}
	if handler.errorSeen {
		c.Set(streamErrorAlreadyRenderedContextKey, true)
	} else if c.Request == nil || c.Request.Context().Err() == nil {
		payload, _ := json.Marshal(map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    "invalid_provider_response",
				"code":    "invalid_provider_response",
				"message": streamErrorClientMessage,
			},
		})
		_, _ = c.Writer.Write([]byte("event: error\ndata: " + string(payload) + "\n\n"))
		c.Writer.Flush()
		c.Set(streamErrorAlreadyRenderedContextKey, true)
	}
	var providerErr *types.OpenAIErrorWithStatusCode
	if errors.As(streamErr, &providerErr) && providerErr != nil {
		return firstResponse, providerErr
	}
	errWithCode := common.ErrorWrapper(streamErr, "invalid_provider_response", http.StatusBadGateway)
	errWithCode.UpstreamAccepted = true
	return firstResponse, errWithCode
}
