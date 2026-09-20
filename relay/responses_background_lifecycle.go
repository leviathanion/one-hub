package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"one-api/common"
	"one-api/common/logger"
	"one-api/common/providerresponse"
	"one-api/common/requestctx"
	commonresponses "one-api/common/responses"
	"one-api/model"
	"one-api/types"
)

func storedBackgroundSink(c *gin.Context, owner *model.ResponseOwner) *backgroundObservationSink {
	if owner == nil || owner.TaskOwnerID == nil {
		return nil
	}
	sink := newBackgroundObservationSink(c.Request.Context(), *owner.TaskOwnerID)
	c.Set(responsesBackgroundObserverDoneContextKey, sink.done)
	return sink
}
func observeStoredBackground(sink *backgroundObservationSink, owner *model.ResponseOwner, raw []byte) {
	if sink == nil || owner == nil {
		return
	}
	var response types.OpenAIResponsesResponses
	if response.DecodeCapturedProviderJSON(raw) == nil && response.ID == owner.ResponseID {
		sink.Submit(&response, nil)
	}
}

// Retrieval/cancel already have an owner. Billing observation must not delay
// their raw body or create another generation billing owner.
func responseStoredLifecycleClient(c *gin.Context, response *http.Response, owner *model.ResponseOwner, policy providerresponse.Policy) *types.OpenAIErrorWithStatusCode {
	sink := storedBackgroundSink(c, owner)
	defer sink.Close()
	if strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		return responseStoredLifecycleStream(c, response, owner, policy, sink)
	}
	if sink == nil || response.StatusCode < 200 || response.StatusCode >= 300 {
		return responseMultipart(c, response, policy)
	}
	capture := &boundedBackgroundCapture{}
	response.Body = &backgroundObservedBody{Reader: io.TeeReader(response.Body, capture), Closer: response.Body}
	apiErr := responseMultipart(c, response, policy)
	if apiErr == nil && !capture.overflow {
		observeStoredBackground(sink, owner, capture.Bytes())
	}
	return apiErr
}

type backgroundObservedBody struct {
	io.Reader
	io.Closer
}
type boundedBackgroundCapture struct {
	bytes.Buffer
	overflow bool
}

func (capture *boundedBackgroundCapture) Write(p []byte) (int, error) {
	if !capture.overflow {
		if len(p) > backgroundPollBodyLimit-capture.Len() {
			capture.overflow = true
			capture.Reset()
		} else {
			_, _ = capture.Buffer.Write(p)
		}
	}
	return len(p), nil
}

func responseStoredLifecycleStream(c *gin.Context, response *http.Response, owner *model.ResponseOwner, policy providerresponse.Policy, sink *backgroundObservationSink) *types.OpenAIErrorWithStatusCode {
	defer response.Body.Close()
	ioOwner, err := newResponsesHTTPIO(c)
	if err != nil {
		return common.ErrorWrapperLocal(err, "response_write_deadline_unsupported", http.StatusInternalServerError)
	}
	defer ioOwner.Close()
	stop := context.AfterFunc(ioOwner.ctx, func() { _ = response.Body.Close() })
	defer stop()
	for key, values := range providerresponse.Filter(response.Header, policy) {
		c.Writer.Header()[key] = append([]string(nil), values...)
	}
	c.Writer.WriteHeader(response.StatusCode)
	framer := commonresponses.NewSSEChunkFramer(responsesStreamMaxEventBytes)
	credentials := requestctx.ProviderCredentials(c)
	deliver := func(event string) (bool, error) {
		if err := ioOwner.WriteEvent(redactProviderSSEEvent(event, credentials...)); err != nil {
			return true, err
		}
		if payload, ok := commonresponses.SSEDataPayload(event); ok && sink != nil {
			var envelope struct {
				Type     string          `json:"type"`
				Response json.RawMessage `json:"response"`
			}
			if json.Unmarshal([]byte(payload), &envelope) == nil && commonresponses.IsResponseLifecycleEvent(envelope.Type) {
				observeStoredBackground(sink, owner, envelope.Response)
			}
		}
		return false, nil
	}
	buffer := make([]byte, 32<<10)
	for {
		n, readErr := response.Body.Read(buffer)
		if n > 0 {
			if _, err := framer.PushChunk(string(buffer[:n]), deliver); err != nil {
				logger.LogError(c.Request.Context(), "stored response stream delivery failed: "+err.Error())
				panic(http.ErrAbortHandler)
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				logger.LogError(c.Request.Context(), "stored response stream read failed: "+readErr.Error())
				panic(http.ErrAbortHandler)
			}
			if pending := framer.TakePending(); pending != "" {
				if _, err := deliver(pending); err != nil {
					panic(http.ErrAbortHandler)
				}
			}
			return nil
		}
	}
}
