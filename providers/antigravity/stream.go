package antigravity

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"

	"one-api/common"
	"one-api/common/requester"
	commonresponses "one-api/common/responses"
	"one-api/providers/antigravity/wire"
	"one-api/types"
)

// antigravityStream 保留 requester 的完整 SSE 事件交付契约，并在物理 EOF
// 后完成协议编码；候选 finishReason 不会提前终止读取尾部用量或错误。
type antigravityStream struct {
	requester.RawSSEEventStream
	framer         *requester.SSEEventFramer
	push           func(string) ([][]byte, error)
	finish         func() ([][]byte, error)
	sanitizeError  func(error) error
	native         bool
	requestContext context.Context
	readContext    context.Context
	readEnded      context.CancelCauseFunc
	data           chan string
	errors         chan error
	done           chan struct{}
	recvOnce       sync.Once
	closeOnce      sync.Once
}

func newAntigravityStream(httpRequester *requester.HTTPRequester, resp *http.Response, decoder *wire.ResponseDecoder, observeUsage func(json.RawMessage, string), sanitizeError func(error) error) (requester.RawSSEEventStream, *types.OpenAIErrorWithStatusCode) {
	return newAntigravityDecoderStream(httpRequester, resp, decoder, observeUsage, sanitizeError, nil)
}

func newAntigravityAggregate(httpRequester *requester.HTTPRequester, resp *http.Response, decoder *wire.ResponseDecoder, observeUsage func(json.RawMessage, string), sanitizeError func(error) error) ([]byte, *types.OpenAIErrorWithStatusCode) {
	var result []byte
	stream, apiErr := newAntigravityDecoderStream(httpRequester, resp, decoder, observeUsage, sanitizeError, func(body []byte) { result = body })
	if apiErr != nil {
		return nil, apiErr
	}
	defer requester.CloseAndDrainStream(stream)
	data, errs := stream.Recv()
	var failure error
	for data != nil || errs != nil {
		select {
		case _, ok := <-data:
			if !ok {
				data = nil
			}
		case err, ok := <-errs:
			if !ok {
				errs = nil
			} else {
				failure = err
			}
		}
	}
	if failure == nil {
		failure = context.Cause(requester.StreamReadContext(stream))
	}
	if errors.Is(failure, io.EOF) && len(result) > 0 {
		return result, nil
	}
	if failure == nil || errors.Is(failure, io.EOF) {
		failure = io.ErrUnexpectedEOF
	}
	if errors.As(failure, &apiErr) {
		return nil, apiErr
	}
	apiErr = common.ErrorWrapper(failure, "invalid_provider_response", http.StatusBadGateway)
	apiErr.UpstreamAccepted = true
	return nil, apiErr
}

func newAntigravityDecoderStream(httpRequester *requester.HTTPRequester, resp *http.Response, decoder *wire.ResponseDecoder, observeUsage func(json.RawMessage, string), sanitizeError func(error) error, acceptResult func([]byte)) (requester.RawSSEEventStream, *types.OpenAIErrorWithStatusCode) {
	push := func(event string) ([][]byte, error) {
		payload, hasData := commonresponses.SSEDataPayload(event)
		if !hasData {
			return nil, nil
		}
		frames, err := decoder.Push([]byte(payload))
		if observeUsage != nil {
			observeUsage(decoder.UsageRaw(), decoder.ResponseModel())
		}
		return frames, err
	}
	finish := func() ([][]byte, error) {
		frames, body, err := decoder.Finish()
		if observeUsage != nil {
			observeUsage(decoder.UsageRaw(), decoder.ResponseModel())
		}
		if err == nil && acceptResult != nil {
			acceptResult(body)
		}
		return frames, err
	}
	return newDecodedAntigravityStream(httpRequester, resp, push, finish, sanitizeError, false)
}

func newAntigravityNativeStream(httpRequester *requester.HTTPRequester, resp *http.Response, decoder *wire.NativeDecoder, observeUsage func(json.RawMessage, string), sanitizeError func(error) error) (requester.RawSSEEventStream, *types.OpenAIErrorWithStatusCode) {
	push := func(event string) ([][]byte, error) {
		payload, hasData := commonresponses.SSEDataPayload(event)
		if !hasData {
			return [][]byte{[]byte(event)}, nil
		}
		inner, err := decoder.Push([]byte(payload))
		if observeUsage != nil {
			observeUsage(decoder.UsageRaw(), decoder.ResponseModel())
		}
		if err != nil {
			return nil, err
		}
		return [][]byte{replaceAntigravitySSEData(event, inner)}, nil
	}
	finish := func() ([][]byte, error) { return nil, decoder.Finish() }
	return newDecodedAntigravityStream(httpRequester, resp, push, finish, sanitizeError, true)
}

func newDecodedAntigravityStream(httpRequester *requester.HTTPRequester, resp *http.Response, push func(string) ([][]byte, error), finish func() ([][]byte, error), sanitizeError func(error) error, native bool) (requester.RawSSEEventStream, *types.OpenAIErrorWithStatusCode) {
	if resp == nil || resp.Body == nil {
		apiErr := common.StringErrorWrapperLocal("Antigravity 上游响应缺少 body", "invalid_provider_response", http.StatusBadGateway)
		apiErr.UpstreamAccepted = true
		return nil, apiErr
	}
	framer := requester.NewSSEEventFramer(wire.MaxRetainedBytes)
	source, apiErr := requester.RequestRawSSEEventStreamWithEmitterOptions(httpRequester, resp, func(line *[]byte, emitter requester.StreamEmitter[string]) {
		event, complete, err := framer.PushLine(*line)
		*line = nil
		if err != nil {
			*line = requester.StreamClosed
			emitter.SendError(err)
			return
		}
		if complete {
			emitter.SendData(string(event))
		}
	}, requester.StreamReadOptions{MaxLineBytes: wire.MaxRetainedBytes})
	if apiErr != nil {
		_ = resp.Body.Close()
		return nil, apiErr
	}
	requestContext := context.Background()
	// resp.Request 持有 transport 派生上下文，正常 body EOF 也会取消它。
	// 下游交付只能跟随原始调用方取消，不能把传输资源回收当成客户端断开。
	if httpRequester != nil && httpRequester.Context != nil {
		requestContext = httpRequester.Context
	}
	readContext, readEnded := context.WithCancelCause(context.Background())
	return &antigravityStream{
		RawSSEEventStream: source, framer: framer, push: push, finish: finish,
		sanitizeError: sanitizeError, native: native, requestContext: requestContext,
		readContext: readContext, readEnded: readEnded,
		data: make(chan string), errors: make(chan error, 1), done: make(chan struct{}),
	}, nil
}

func (s *antigravityStream) Recv() (<-chan string, <-chan error) {
	s.recvOnce.Do(func() { go s.run() })
	return s.data, s.errors
}

func (s *antigravityStream) run() {
	defer close(s.errors)
	defer close(s.data)
	defer requester.CloseAndDrainStream(s.RawSSEEventStream)
	defer func() {
		if recover() != nil {
			s.fail(errors.New("Antigravity 流处理异常"))
		}
	}()
	stop := context.AfterFunc(s.requestContext, s.Close)
	defer stop()
	if s.requestContext.Err() != nil {
		s.Close()
		return
	}
	data, errs := s.RawSSEEventStream.Recv()
	// requester 的 DataChan 无缓冲，故后续读取错误不可能越过尚未接收的事件。
	for data != nil || errs != nil {
		select {
		case <-s.done:
			return
		case event, ok := <-data:
			if !ok {
				data = nil
				continue
			}
			frames, err := s.push(event)
			if !s.sendFrames(frames) {
				return
			}
			if err != nil {
				s.fail(err)
				return
			}
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			if errors.Is(err, io.EOF) {
				if s.framer.BufferedLen() != 0 {
					s.fail(io.ErrUnexpectedEOF)
					return
				}
				frames, finishErr := s.finish()
				if finishErr != nil {
					s.fail(finishErr)
					return
				}
				if !s.sendFrames(frames) {
					return
				}
				s.readEnded(io.EOF)
				s.errors <- io.EOF
				return
			}
			s.fail(err)
			return
		}
	}
	// 通道关闭不等同于正常 EOF，尤其不能把主动 Close 解释为生成成功。
	s.fail(io.ErrUnexpectedEOF)
}

func (s *antigravityStream) sendFrames(frames [][]byte) bool {
	select {
	case <-s.done:
		return false
	case <-s.requestContext.Done():
		s.Close()
		return false
	default:
	}
	for _, frame := range frames {
		select {
		case <-s.done:
			return false
		case <-s.requestContext.Done():
			s.Close()
			return false
		default:
		}
		select {
		case <-s.done:
			return false
		case <-s.requestContext.Done():
			s.Close()
			return false
		case s.data <- string(frame):
		}
	}
	return true
}

func (s *antigravityStream) fail(err error) {
	if err == nil {
		err = io.ErrUnexpectedEOF
	}
	var upstreamErr *wire.UpstreamError
	nativeUpstreamError := s.native && errors.As(err, &upstreamErr)
	if s.sanitizeError != nil {
		if sanitized := s.sanitizeError(err); sanitized != nil {
			err = sanitized
		}
	}
	if nativeUpstreamError {
		var apiErr *types.OpenAIErrorWithStatusCode
		if errors.As(err, &apiErr) && json.Valid(apiErr.RawBody) {
			if !s.sendFrames([][]byte{replaceAntigravitySSEData("data:\n\n", apiErr.RawBody)}) {
				return
			}
		}
	}
	s.readEnded(err)
	select {
	case <-s.done:
	case s.errors <- err:
	}
}

func (s *antigravityStream) Close() {
	s.closeOnce.Do(func() {
		cause := error(io.ErrClosedPipe)
		if s.requestContext.Err() != nil {
			cause = context.Cause(s.requestContext)
		}
		s.readEnded(cause)
		close(s.done)
		s.RawSSEEventStream.Close()
	})
}

func (s *antigravityStream) CloseAndDrain() {
	_ = s.CloseAndDrainContext(context.Background())
}

func (s *antigravityStream) CloseAndDrainContext(ctx context.Context) error {
	data, errs := s.Recv()
	s.Close()
	for data != nil || errs != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case _, ok := <-data:
			if !ok {
				data = nil
			}
		case _, ok := <-errs:
			if !ok {
				errs = nil
			}
		}
	}
	return nil
}

func (s *antigravityStream) ReadContext() context.Context { return s.readContext }

// 内层 JSON 可以含换行；每行都必须重新加 data 前缀，防止变成 SSE 字段。
// 事件的其他字段和注释保持原样，多个原 data 字段合并为一个逻辑 payload。
func replaceAntigravitySSEData(event string, payload []byte) []byte {
	var result strings.Builder
	replaced := false
	for event != "" {
		line, rest := requester.SplitSSELine(event)
		event = rest
		content := requester.SSELineContent(line)
		if content != "data" && !strings.HasPrefix(content, "data:") {
			result.WriteString(line)
			continue
		}
		if replaced {
			continue
		}
		replaced = true
		for _, payloadLine := range strings.Split(string(payload), "\n") {
			result.WriteString("data: ")
			result.WriteString(payloadLine)
			result.WriteByte('\n')
		}
	}
	return []byte(result.String())
}
