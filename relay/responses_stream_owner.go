package relay

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"one-api/common"
	"one-api/common/logger"
	"one-api/common/requestctx"
	"one-api/common/requester"
	commonresponses "one-api/common/responses"
	"one-api/relay/relay_util"
	"one-api/types"
)

const storedResponsesStreamBarrierMaxBytes = 1 << 20
const responsesStreamMaxEventBytes = 16 << 20
const streamErrorAlreadyRenderedContextKey = "stream_error_already_rendered"
const responsesStreamErrorAlreadyRenderedContextKey = streamErrorAlreadyRenderedContextKey
const responsesHTTPIOContextKey = "responses_http_io"

func newResponsesSSEEventFramer(maxBytes int) *commonresponses.SSEChunkFramer {
	return commonresponses.NewSSEChunkFramer(maxBytes)
}

func responseNativeResponsesStreamClient(c *gin.Context, stream commonresponses.EventStream, observer *commonresponses.StreamObserver) (time.Time, *types.OpenAIErrorWithStatusCode) {
	return responseResponsesStreamClient(c, stream, observer, 0, false)
}

func responseStoredResponsesStreamClient(c *gin.Context, stream commonresponses.EventStream, observer *commonresponses.StreamObserver, channelID int) (time.Time, *types.OpenAIErrorWithStatusCode) {
	return responseResponsesStreamClient(c, stream, observer, channelID, true)
}

// 同一个读取循环分别拥有证据观察和交付许可；业务 terminal 不结束传输。
func responseResponsesStreamClient(c *gin.Context, stream commonresponses.EventStream, observer *commonresponses.StreamObserver, channelID int, durable bool) (time.Time, *types.OpenAIErrorWithStatusCode) {
	defer func() {
		ctx, cancel := boundedResponsesLifecycleContext(context.WithoutCancel(c.Request.Context()))
		defer cancel()
		if err := requester.CloseAndDrainStreamContext(ctx, stream); err != nil {
			logger.LogWarn(ctx, "Responses stream cleanup deadline exceeded")
		}
	}()
	var ioOwner *responsesHTTPIO
	if value, exists := c.Get(responsesHTTPIOContextKey); exists {
		ioOwner, _ = value.(*responsesHTTPIO)
	}
	ctx := c.Request.Context()
	if ioOwner != nil {
		ctx = ioOwner.ctx
		defer ioOwner.WatchStream(stream)()
	}
	credentials := requestctx.ProviderCredentials(c)
	framer := newResponsesSSEEventFramer(responsesStreamMaxEventBytes)
	var firstResponseTime time.Time
	var failure *types.OpenAIErrorWithStatusCode
	var cleanupDeadline time.Time
	var buffered strings.Builder
	ownerPersisted := !durable
	headersReady := false

	prepareHeaders := func() {
		if headersReady {
			return
		}
		applyProviderResponseHeaders(c)
		requester.SetEventStreamHeaders(c)
		c.Writer.WriteHeader(providerResponseStatus(c, http.StatusOK))
		headersReady = true
	}
	fail := func(apiErr *types.OpenAIErrorWithStatusCode) {
		if failure != nil {
			return
		}
		failure = apiErr
		failure.UpstreamAccepted = !observer.ProviderRejected()
		cleanupDeadline = time.Now().Add(responsesLifecycleIOTimeout)
		stream.Close()
		if ioOwner != nil {
			ioOwner.Stop()
		}
	}
	write := func(raw string) {
		if failure != nil || raw == "" {
			return
		}
		prepareHeaders()
		var err error
		if ioOwner != nil {
			err = ioOwner.WriteEvent(raw)
		} else {
			// 直接调用消费者的单元测试也观察真实 write/flush 错误。
			_, err = io.WriteString(c.Writer, raw)
			if err == nil {
				err = http.NewResponseController(c.Writer).Flush()
			}
		}
		if err != nil {
			fail(common.ErrorWrapperLocal(err, "write_response_body_failed", http.StatusInternalServerError))
		}
	}
	deliver := func(raw string) {
		if failure != nil {
			return
		}
		raw = redactProviderSSEEvent(raw, credentials...)
		if ownerPersisted {
			write(raw)
			return
		}
		if len(raw) > storedResponsesStreamBarrierMaxBytes-buffered.Len() {
			fail(common.StringErrorWrapperLocal("provider stream did not establish a stored response id within the buffering limit", "invalid_provider_response", http.StatusBadGateway))
			return
		}
		buffered.WriteString(raw)
		if id := observer.ObservedResponseID(); id != "" {
			if err := persistStoredResponseOwner(c, id, channelID); err != nil {
				fail(err)
				return
			}
			ownerPersisted = true
		} else if observer.TerminalKind() != commonresponses.StreamTerminalError {
			return
		}
		write(buffered.String())
		buffered.Reset()
	}
	observe := func(event string) (bool, error) {
		if !cleanupDeadline.IsZero() && time.Now().After(cleanupDeadline) {
			return true, context.DeadlineExceeded
		}
		if _, hasData := commonresponses.SSEDataPayload(event); !hasData {
			deliver(event)
			return false, nil
		}
		if err := observer.ObserveEvent(event); err != nil {
			return true, err
		}
		if err := stream.ObserveResponsesEvent(event); err != nil {
			return true, err
		}
		deliver(event)
		// owner/write 失败后继续观察已取得 chunk；不再接收下一 chunk。
		return false, nil
	}
	finish := func(streamErr error) (time.Time, *types.OpenAIErrorWithStatusCode) {
		if failure == nil && ctx.Err() == nil {
			// EOF 不补事件；原始尾部仍经过同一个安全和 owner 屏障。
			deliver(framer.TakePending())
		}
		if failure == nil {
			// 下游停止会取消 I/O context；真实读取原因仍由 reader 持有。
			if source := requester.StreamReadContext(stream); source != nil {
				if cause := context.Cause(source); cause != nil && !errors.Is(cause, io.EOF) && !errors.Is(cause, io.ErrClosedPipe) {
					streamErr = cause
				}
			}
			switch {
			case c.Request.Context().Err() != nil:
				fail(responsesStreamClientCanceledError())
			case errors.Is(ctx.Err(), context.DeadlineExceeded):
				fail(common.ErrorWrapperLocal(ctx.Err(), "stream_timeout", http.StatusGatewayTimeout))
			case streamErr != nil && !errors.Is(streamErr, io.EOF):
				fail(common.ErrorWrapperLocal(streamErr, relay_util.ResponsesStreamFailureCode(streamErr), http.StatusBadGateway))
			case ctx.Err() != nil:
				fail(common.ErrorWrapperLocal(ctx.Err(), "stream_interrupted", http.StatusBadGateway))
			case !ownerPersisted && observer.TerminalKind() != commonresponses.StreamTerminalError:
				fail(common.StringErrorWrapperLocal("provider stream ended before establishing a stored response id", "invalid_provider_response", http.StatusBadGateway))
			}
		}
		if failure != nil {
			if c.Writer.Written() {
				c.Set(responsesStreamErrorAlreadyRenderedContextKey, true)
				logger.LogError(c.Request.Context(), "Responses stream interrupted: "+failure.Message)
				// HTTP ingress 在返回本地失败后中止已提交响应。
			}
			return firstResponseTime, failure
		}
		prepareHeaders()
		if err := responsesProviderTerminalError(c, observer); err != nil {
			return firstResponseTime, err
		}
		return firstResponseTime, nil
	}

	dataChan, errChan := stream.Recv()
	for dataChan != nil || errChan != nil {
		select {
		case <-ctx.Done():
			return finish(ctx.Err())
		case data, ok := <-dataChan:
			if !ok {
				dataChan = nil
				continue
			}
			if firstResponseTime.IsZero() {
				firstResponseTime = time.Now()
			}
			_, err := framer.PushChunk(data, observe)
			if err != nil {
				fail(common.ErrorWrapperLocal(err, relay_util.ResponsesStreamFailureCode(err), http.StatusBadGateway))
			}
			if failure != nil {
				return finish(nil)
			}
		case err, ok := <-errChan:
			if !ok {
				errChan = nil
				continue
			}
			return finish(err)
		}
	}
	return finish(nil)
}

func responsesProviderTerminalError(c *gin.Context, observer *commonresponses.StreamObserver) *types.OpenAIErrorWithStatusCode {
	if observer == nil || observer.TerminalKind() != commonresponses.StreamTerminalError {
		return nil
	}
	streamError := observer.TerminalError()
	code, message := "upstream_error", streamErrorClientMessage
	if streamError != nil {
		if strings.TrimSpace(streamError.Code) != "" {
			code = streamError.Code
		}
		if strings.TrimSpace(streamError.Message) != "" {
			message = streamError.Message
		}
	}
	apiErr := common.StringErrorWrapper(message, code, http.StatusBadGateway)
	if streamError != nil && streamError.Param != nil {
		apiErr.Param = *streamError.Param
	}
	c.Set(responsesStreamErrorAlreadyRenderedContextKey, true)
	return apiErr
}

func responsesStreamClientCanceledError() *types.OpenAIErrorWithStatusCode {
	return common.StringErrorWrapperLocal("request was canceled after the provider accepted the response", "request_canceled", 499)
}
