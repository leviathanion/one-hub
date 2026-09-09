package relay

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"one-api/common"
	"one-api/common/logger"
	"one-api/common/providerresponse"
	"one-api/common/requester"
	commonresponses "one-api/common/responses"
	"one-api/relay/relay_util"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

const storedResponsesStreamBarrierMaxBytes = 1 << 20
const responsesStreamMaxEventBytes = 16 << 20
const streamErrorAlreadyRenderedContextKey = "stream_error_already_rendered"

var errResponsesStreamIncompleteEvent = errors.New("provider Responses stream ended with an incomplete SSE event")

func newResponsesSSEEventFramer(maxBytes int) *commonresponses.SSEChunkFramer {
	return commonresponses.NewSSEChunkFramer(maxBytes)
}

// Keep the protocol-local name as an alias while the Responses owner is
// migrated; the receipt itself is transport-wide, not Responses-specific.
const responsesStreamErrorAlreadyRenderedContextKey = streamErrorAlreadyRenderedContextKey

func responseNativeResponsesStreamClient(c *gin.Context, stream commonresponses.EventStream, observer *commonresponses.StreamObserver) (time.Time, *types.OpenAIErrorWithStatusCode) {
	applyProviderResponseHeaders(c)
	requester.SetEventStreamHeaders(c)
	c.Writer.WriteHeader(providerResponseStatus(c, http.StatusOK))
	dataChan, errChan := stream.Recv()
	defer requester.CloseAndDrainStream(stream)

	streamWriter := relay_util.NewBufferedStreamWriter(c.Writer, 0)
	defer streamWriter.Close()
	framer := newResponsesSSEEventFramer(responsesStreamMaxEventBytes)
	var firstResponseTime time.Time
	var deliveryErr error

	fail := func(streamErr error) (time.Time, *types.OpenAIErrorWithStatusCode) {
		if deliveryErr != nil {
			c.Set(responsesStreamErrorAlreadyRenderedContextKey, true)
			return firstResponseTime, common.ErrorWrapperLocal(deliveryErr, "write_response_body_failed", http.StatusInternalServerError)
		}
		writeResponsesStreamProtocolError(c, observer.ReliableNextSequenceNumber(), streamErr)
		c.Set(responsesStreamErrorAlreadyRenderedContextKey, true)
		logger.LogError(c.Request.Context(), "Responses stream lifecycle error: "+common.RedactSensitiveText(streamErr.Error()))
		code, message := responsesStreamProtocolFailure(streamErr, "provider Responses stream ended without a valid terminal")
		return firstResponseTime, common.StringErrorWrapperLocal(message, code, http.StatusBadGateway)
	}
	finish := func(streamErr error) (time.Time, *types.OpenAIErrorWithStatusCode) {
		if c.Request.Context().Err() != nil {
			return firstResponseTime, responsesStreamClientCanceledError()
		}
		if framer.HasPending() {
			framer.Reset()
			if streamErr == nil || errors.Is(streamErr, io.EOF) {
				streamErr = errResponsesStreamIncompleteEvent
			}
		}
		if streamErr == nil || errors.Is(streamErr, io.EOF) {
			streamErr = observer.StreamCompletionError()
		}
		if streamErr != nil {
			return fail(streamErr)
		}
		if providerErr := responsesProviderTerminalError(c, observer); providerErr != nil {
			return firstResponseTime, providerErr
		}
		return firstResponseTime, nil
	}
	handleData := func(data string) (bool, error) {
		if firstResponseTime.IsZero() {
			firstResponseTime = time.Now()
		}
		return framer.PushChunk(data, func(event string) (bool, error) {
			if err := observer.AcceptRawEvent(event, func() error {
				return stream.ObserveAcceptedResponsesEvent(event)
			}); err != nil {
				return true, err
			}
			event = sanitizeProviderSSEEvent(event)
			if _, err := streamWriter.WriteString(event); err != nil {
				deliveryErr = err
				return true, err
			}
			return observer.TerminalSeen(), nil
		})
	}

	dataOpen := dataChan != nil
	errOpen := errChan != nil
	for dataOpen || errOpen {
		if dataOpen {
			select {
			case data, ok := <-dataChan:
				if !ok {
					dataOpen = false
					dataChan = nil
					continue
				}
				stop, err := handleData(data)
				if err != nil {
					return fail(err)
				}
				if stop {
					return finish(nil)
				}
				continue
			default:
			}
		}

		select {
		case <-c.Request.Context().Done():
			return firstResponseTime, responsesStreamClientCanceledError()
		case data, ok := <-dataChan:
			if !ok {
				dataOpen = false
				dataChan = nil
				continue
			}
			stop, err := handleData(data)
			if err != nil {
				return fail(err)
			}
			if stop {
				return finish(nil)
			}
		case streamErr, ok := <-errChan:
			if !ok {
				errOpen = false
				errChan = nil
				continue
			}
			return finish(streamErr)
		}
	}
	return finish(nil)
}

func responsesProviderTerminalError(c *gin.Context, observer *commonresponses.StreamObserver) *types.OpenAIErrorWithStatusCode {
	if observer == nil || observer.TerminalKind() != commonresponses.StreamTerminalError {
		return nil
	}
	streamError := observer.TerminalError()
	if streamError == nil {
		return common.StringErrorWrapper(streamErrorClientMessage, "upstream_error", http.StatusBadGateway)
	}
	code := strings.TrimSpace(streamError.Code)
	if code == "" {
		code = "upstream_error"
	}
	message := strings.TrimSpace(streamError.Message)
	if message == "" {
		message = streamErrorClientMessage
	}
	apiErr := common.StringErrorWrapper(message, code, http.StatusBadGateway)
	if streamError.Param != nil {
		apiErr.Param = *streamError.Param
	}
	// The provider event has already been delivered byte-for-byte. The outer
	// error path only needs the failure fact for quota settlement.
	c.Set(responsesStreamErrorAlreadyRenderedContextKey, true)
	return providerresponse.SanitizeAPIError(apiErr)
}

func writeResponsesStreamProtocolError(c *gin.Context, sequenceNumber *int64, failure error) {
	if c == nil || c.Request == nil || c.Request.Context().Err() != nil {
		return
	}
	_, _ = c.Writer.Write([]byte(responsesStreamProtocolErrorEvent(sequenceNumber, failure)))
	c.Writer.Flush()
}

func responsesStreamProtocolErrorEvent(sequenceNumber *int64, failure error) string {
	code, message := responsesStreamProtocolFailure(failure, streamErrorClientMessage)
	payload := map[string]any{
		"type":    "error",
		"code":    code,
		"message": message,
		"param":   nil,
	}
	if sequenceNumber != nil {
		payload["sequence_number"] = *sequenceNumber
	}
	encoded, _ := json.Marshal(payload)
	return "event: error\ndata: " + string(encoded) + "\n\n"
}

func responsesStreamProtocolFailure(err error, fallbackMessage string) (string, string) {
	code := relay_util.ResponsesStreamFailureCode(err)
	if code == "provider_usage_state_limit" {
		return code, "provider stream state limit exceeded"
	}
	return code, fallbackMessage
}

// responseStoredResponsesStreamClient delays every downstream byte until an
// ID-bearing provider event has a durable owner. A valid provider emits
// response.created first, so the bounded buffer is a guard against malformed
// streams rather than a normal buffering strategy.
func responseStoredResponsesStreamClient(c *gin.Context, stream commonresponses.EventStream, observer *commonresponses.StreamObserver, channelID int) (time.Time, *types.OpenAIErrorWithStatusCode) {
	dataChan, errChan := stream.Recv()
	defer requester.CloseAndDrainStream(stream)

	var firstResponseTime time.Time
	buffered := make([]string, 0, 4)
	bufferedBytes := 0
	ownerPersisted := false
	eventFramer := newResponsesSSEEventFramer(responsesStreamMaxEventBytes)
	var streamWriter *relay_util.BufferedStreamWriter
	var deliveryErr error
	defer func() {
		if streamWriter != nil {
			_ = streamWriter.Close()
		}
	}()
	writeDownstream := func(data string) error {
		if deliveryErr != nil {
			return deliveryErr
		}
		if streamWriter == nil {
			return errors.New("responses stream writer is not initialized")
		}
		if _, err := streamWriter.WriteString(data); err != nil {
			deliveryErr = err
			c.Set(responsesStreamErrorAlreadyRenderedContextKey, true)
			return err
		}
		return nil
	}

	flushBuffered := func() *types.OpenAIErrorWithStatusCode {
		applyProviderResponseHeaders(c)
		requester.SetEventStreamHeaders(c)
		c.Writer.WriteHeader(providerResponseStatus(c, http.StatusOK))
		streamWriter = relay_util.NewBufferedStreamWriter(c.Writer, 0)
		for _, data := range buffered {
			if err := writeDownstream(data); err != nil {
				return common.ErrorWrapperLocal(err, "write_response_body_failed", http.StatusInternalServerError)
			}
		}
		buffered = nil
		bufferedBytes = 0
		return nil
	}

	commitOwnerAndFlush := func(responseID string) *types.OpenAIErrorWithStatusCode {
		if err := persistStoredResponseOwner(c, responseID, channelID); err != nil {
			return err
		}
		ownerPersisted = true
		if err := flushBuffered(); err != nil {
			return err
		}
		return nil
	}

	handleEvent := func(data string) *types.OpenAIErrorWithStatusCode {
		if firstResponseTime.IsZero() {
			firstResponseTime = time.Now()
		}
		if lifecycleErr := observer.AcceptRawEvent(data, func() error {
			return stream.ObserveAcceptedResponsesEvent(data)
		}); lifecycleErr != nil {
			if ownerPersisted {
				_ = writeDownstream(responsesStreamProtocolErrorEvent(observer.ReliableNextSequenceNumber(), lifecycleErr))
				c.Set(responsesStreamErrorAlreadyRenderedContextKey, true)
			}
			logger.LogError(c.Request.Context(), "Responses stream lifecycle error: "+common.RedactSensitiveText(lifecycleErr.Error()))
			code, message := responsesStreamProtocolFailure(lifecycleErr, "provider Responses stream contains an invalid lifecycle")
			return common.StringErrorWrapperLocal(message, code, http.StatusBadGateway)
		}
		data = sanitizeProviderSSEEvent(data)
		if ownerPersisted {
			select {
			case <-c.Request.Context().Done():
				return nil
			default:
				if err := writeDownstream(data); err != nil {
					return common.ErrorWrapperLocal(err, "write_response_body_failed", http.StatusInternalServerError)
				}
			}
			return nil
		}
		buffered = append(buffered, data)
		bufferedBytes += len(data)
		if bufferedBytes > storedResponsesStreamBarrierMaxBytes {
			return common.StringErrorWrapperLocal("provider stream did not establish a stored response id within the buffering limit", "invalid_provider_response", http.StatusBadGateway)
		}
		if final := observer.FinalResponse(); final != nil && final.ID != "" {
			return commitOwnerAndFlush(final.ID)
		}
		if observer.TerminalKind() == commonresponses.StreamTerminalError {
			return flushBuffered()
		}
		return nil
	}

	handleData := func(data string) *types.OpenAIErrorWithStatusCode {
		if firstResponseTime.IsZero() {
			firstResponseTime = time.Now()
		}
		var eventErr *types.OpenAIErrorWithStatusCode
		_, framingErr := eventFramer.PushChunk(data, func(event string) (bool, error) {
			eventErr = handleEvent(event)
			return eventErr != nil || observer.TerminalSeen(), nil
		})
		if eventErr != nil {
			return eventErr
		}
		if framingErr == nil {
			return nil
		}
		if ownerPersisted {
			_ = writeDownstream(responsesStreamProtocolErrorEvent(observer.ReliableNextSequenceNumber(), framingErr))
			c.Set(responsesStreamErrorAlreadyRenderedContextKey, true)
		}
		code, message := responsesStreamProtocolFailure(framingErr, "provider Responses stream contains an invalid SSE event")
		return common.StringErrorWrapperLocal(message, code, http.StatusBadGateway)
	}

	handleCommittedError := func(err error) {
		_ = writeDownstream(responsesStreamProtocolErrorEvent(observer.ReliableNextSequenceNumber(), err))
		if err != nil && !errors.Is(err, io.EOF) {
			logger.LogError(c.Request.Context(), "Stream err:"+common.RedactSensitiveText(err.Error()))
		}
	}

	dataOpen := dataChan != nil
	errOpen := errChan != nil
	for dataOpen || errOpen {
		select {
		case <-c.Request.Context().Done():
			return firstResponseTime, responsesStreamClientCanceledError()
		case data, ok := <-dataChan:
			if !ok {
				dataOpen = false
				dataChan = nil
				continue
			}
			if apiErr := handleData(data); apiErr != nil {
				return firstResponseTime, apiErr
			}
			if observer.TerminalSeen() {
				if lifecycleErr := observer.StreamCompletionError(); lifecycleErr != nil {
					if ownerPersisted {
						handleCommittedError(lifecycleErr)
						c.Set(responsesStreamErrorAlreadyRenderedContextKey, true)
					}
					code, message := responsesStreamProtocolFailure(lifecycleErr, "provider Responses stream ended with an invalid terminal")
					return firstResponseTime, common.StringErrorWrapperLocal(message, code, http.StatusBadGateway)
				}
				if providerErr := responsesProviderTerminalError(c, observer); providerErr != nil {
					return firstResponseTime, providerErr
				}
				return firstResponseTime, nil
			}
		case streamErr, ok := <-errChan:
			if !ok {
				errOpen = false
				errChan = nil
				continue
			}
			if eventFramer.HasPending() {
				eventFramer.Reset()
				if streamErr == nil || errors.Is(streamErr, io.EOF) {
					streamErr = errResponsesStreamIncompleteEvent
				}
			}
			if ownerPersisted {
				if streamErr == nil || errors.Is(streamErr, io.EOF) {
					if lifecycleErr := observer.StreamCompletionError(); lifecycleErr == nil {
						return firstResponseTime, nil
					} else {
						streamErr = lifecycleErr
					}
				}
				handleCommittedError(streamErr)
				c.Set(responsesStreamErrorAlreadyRenderedContextKey, true)
				code, message := responsesStreamProtocolFailure(streamErr, "provider Responses stream ended without a valid terminal")
				return firstResponseTime, common.StringErrorWrapperLocal(message, code, http.StatusBadGateway)
			}
			if streamErr == nil || errors.Is(streamErr, io.EOF) {
				return firstResponseTime, common.StringErrorWrapperLocal("provider stream ended before establishing a stored response id", "invalid_provider_response", http.StatusBadGateway)
			}
			if code := relay_util.ResponsesStreamFailureCode(streamErr); code != "invalid_provider_response" {
				_, message := responsesStreamProtocolFailure(streamErr, "provider Responses stream tracking failed")
				return firstResponseTime, common.StringErrorWrapperLocal(message, code, http.StatusBadGateway)
			}
			return firstResponseTime, common.ErrorWrapperLocal(streamErr, "stream_read_failed", http.StatusBadGateway)
		}
	}
	if eventFramer.HasPending() {
		eventFramer.Reset()
		if ownerPersisted {
			handleCommittedError(errResponsesStreamIncompleteEvent)
			c.Set(responsesStreamErrorAlreadyRenderedContextKey, true)
			return firstResponseTime, common.StringErrorWrapperLocal("provider Responses stream ended with an incomplete SSE event", "invalid_provider_response", http.StatusBadGateway)
		}
		return firstResponseTime, common.StringErrorWrapperLocal("provider stream ended before establishing a stored response id", "invalid_provider_response", http.StatusBadGateway)
	}
	if !ownerPersisted {
		return firstResponseTime, common.StringErrorWrapperLocal("provider stream ended before establishing a stored response id", "invalid_provider_response", http.StatusBadGateway)
	}
	if lifecycleErr := observer.StreamCompletionError(); lifecycleErr != nil {
		_ = writeDownstream(responsesStreamProtocolErrorEvent(observer.ReliableNextSequenceNumber(), lifecycleErr))
		c.Set(responsesStreamErrorAlreadyRenderedContextKey, true)
		code, message := responsesStreamProtocolFailure(lifecycleErr, "provider Responses stream ended without a valid terminal")
		return firstResponseTime, common.StringErrorWrapperLocal(message, code, http.StatusBadGateway)
	}
	return firstResponseTime, nil
}

func responsesStreamClientCanceledError() *types.OpenAIErrorWithStatusCode {
	return common.StringErrorWrapperLocal("request was canceled after the provider accepted the response", "request_canceled", 499)
}
