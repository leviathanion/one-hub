package requester

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"one-api/common/logger"
	"one-api/types"
	"runtime"
	"runtime/debug"
	"sync"
	"time"

	"github.com/bytedance/gopkg/util/gopool"
)

var StreamClosed = []byte("stream_closed")

var ErrStreamLineTooLarge = errors.New("stream line exceeds configured read limit")

// ErrStreamProtocolTerminalMissing means a stream whose protocol promises an
// explicit terminal marker reached the transport EOF without observing it.
var ErrStreamProtocolTerminalMissing = errors.New("provider stream ended without protocol terminal")

var streamErrorSendTimeout = time.Second

const defaultProviderStreamMaxLineBytes int64 = 16 << 20

type HandlerPrefix[T streamable] func(rawLine *[]byte, dataChan chan T, errChan chan error)
type HandlerPrefixWithEmitter[T streamable] func(rawLine *[]byte, emitter StreamEmitter[T])

type streamable interface {
	// types.ChatCompletionStreamResponse | types.CompletionResponse
	any
}

type StreamReaderInterface[T streamable] interface {
	Recv() (<-chan T, <-chan error)
	// Close must be idempotent and safe to call concurrently. Implementations
	// should make the channels returned by Recv stop blocking or close promptly.
	Close()
}

// CloseAndDrainStream closes a stream and, when supported by its concrete
// implementation, drains the delivery channels until the reader exits. The
// drain is what releases legacy handlers that may already be blocked in a raw
// channel send when their downstream consumer stops early.
func CloseAndDrainStream[T streamable](stream StreamReaderInterface[T]) {
	if stream == nil {
		return
	}
	if drainable, ok := stream.(interface{ CloseAndDrain() }); ok {
		drainable.CloseAndDrain()
		return
	}
	stream.Close()
}

func CloseAndDrainStreamContext[T streamable](ctx context.Context, stream StreamReaderInterface[T]) error {
	if stream == nil {
		return nil
	}
	if drainable, ok := stream.(interface{ CloseAndDrainContext(context.Context) error }); ok {
		return drainable.CloseAndDrainContext(ctx)
	}
	stream.Close()
	return nil
}

type streamReader[T streamable] struct {
	reader   *bufio.Reader
	response *http.Response
	NoTrim   bool
	options  StreamReadOptions

	handlerPrefix        HandlerPrefix[T]
	handlerPrefixEmitter HandlerPrefixWithEmitter[T]

	DataChan    chan T
	ErrChan     chan error
	done        chan struct{}
	closeOnce   sync.Once
	recvOnce    sync.Once
	finishOnce  sync.Once
	readContext context.Context
	readEnded   context.CancelCauseFunc
}

type StreamReadOptions struct {
	MaxLineBytes            int64
	SSELines                bool
	RequireProtocolTerminal bool
	// ProtocolTerminalPredicate is consulted only for transport EOF. A true
	// result preserves io.EOF as the handler's valid protocol terminal while
	// all other read errors, including timeouts, keep their original meaning.
	ProtocolTerminalPredicate func() bool
}

func normalizeStreamReadOptions(options StreamReadOptions) StreamReadOptions {
	if options.MaxLineBytes <= 0 {
		options.MaxLineBytes = defaultProviderStreamMaxLineBytes
	}
	return options
}

type StreamEmitter[T streamable] struct {
	stream *streamReader[T]
}

func (e StreamEmitter[T]) SendData(data T) bool {
	if e.stream == nil {
		return false
	}
	select {
	case e.stream.DataChan <- data:
		return true
	case <-streamDone(e.stream):
		return false
	}
}

func (e StreamEmitter[T]) SendError(err error) bool {
	return sendStreamError(e.stream, err)
}

func (stream *streamReader[T]) Recv() (<-chan T, <-chan error) {
	if stream == nil {
		return nil, nil
	}
	stream.recvOnce.Do(func() {
		gopool.Go(func() {
			defer stream.finish()
			defer func() {
				if r := recover(); r != nil {
					stack := debug.Stack()
					// This generic stream reader may process provider payloads from
					// many relay modes. Keep default panic diagnostics to safe
					// metadata; mode-specific code can add guarded debug traces.
					logger.SysError(fmt.Sprintf("stream reader panic: class=%s stack_hash=%s", streamReaderPanicClass(r), streamReaderStackHash(stack)))

					err := &types.OpenAIError{
						Code:    "system error",
						Message: "stream processing panic",
						Type:    "system_error",
					}

					stream.sendErr(err)
				}
			}()
			stream.processLines()
		})
	})

	return stream.DataChan, stream.ErrChan
}

//nolint:gocognit
func (stream *streamReader[T]) processLines() {
	for {
		rawLine, readErr := stream.readLine()
		if !stream.NoTrim {
			rawLine = bytes.TrimSpace(rawLine)
		}
		// Reader 可以同时返回字节和错误；先有序移交这些字节。
		if len(rawLine) > 0 {
			if stream.handlerPrefixEmitter != nil {
				stream.handlerPrefixEmitter(&rawLine, StreamEmitter[T]{stream: stream})
			} else if stream.handlerPrefix != nil {
				stream.handlerPrefix(&rawLine, stream.DataChan, stream.ErrChan)
			}
			select {
			case <-streamDone(stream):
				return
			default:
			}
			if bytes.Equal(rawLine, StreamClosed) {
				return
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) && stream.options.RequireProtocolTerminal {
				terminalObserved := stream.options.ProtocolTerminalPredicate != nil && stream.options.ProtocolTerminalPredicate()
				if !terminalObserved {
					readErr = ErrStreamProtocolTerminalMissing
				}
			}
			stream.sendErr(readErr)
			if errors.Is(readErr, ErrStreamLineTooLarge) {
				stream.Close()
			}
			return
		}
	}
}

func (stream *streamReader[T]) readLine() ([]byte, error) {
	if stream == nil || stream.reader == nil {
		return nil, io.ErrClosedPipe
	}
	if stream.options.SSELines {
		return stream.readSSELine()
	}
	limit := stream.options.MaxLineBytes
	if limit <= 0 {
		line, err := stream.reader.ReadBytes('\n')
		if len(line) > 0 && errors.Is(err, io.EOF) {
			return line, nil
		}
		return line, err
	}

	var line []byte
	for {
		fragment, err := stream.reader.ReadSlice('\n')
		if len(fragment) > 0 {
			if int64(len(line)+len(fragment)) > limit {
				return nil, ErrStreamLineTooLarge
			}
			line = append(line, fragment...)
		}
		if err == nil {
			return line, nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if len(line) > 0 && errors.Is(err, io.EOF) {
			return line, nil
		}
		return line, err
	}
}

// readSSELine 不等待 CR 后的下一字节，避免 CR-only 事件被缓冲至 EOF。
func (stream *streamReader[T]) readSSELine() ([]byte, error) {
	var line []byte
	for {
		if _, err := stream.reader.Peek(1); err != nil {
			return line, err
		}
		buffered, _ := stream.reader.Peek(stream.reader.Buffered())
		segment, _ := SplitSSELine(string(buffered))
		if limit := stream.options.MaxLineBytes; limit > 0 && int64(len(line)+len(segment)) > limit {
			return nil, ErrStreamLineTooLarge
		}
		line = append(line, segment...)
		_, _ = stream.reader.Discard(len(segment))
		last := segment[len(segment)-1]
		if last == '\r' || last == '\n' {
			return line, nil
		}
	}
}

func (stream *streamReader[T]) Close() {
	if stream == nil {
		return
	}
	stream.closeOnce.Do(func() {
		if stream.readEnded != nil {
			stream.readEnded(io.ErrClosedPipe)
		}
		if stream.done != nil {
			close(stream.done)
		}
		if stream.response != nil && stream.response.Body != nil {
			_ = stream.response.Body.Close()
		}
	})
}

// ReadContext 的 cause 只表示读取事实，正常 EOF 与主动 Close 保持可区分。
func (stream *streamReader[T]) ReadContext() context.Context {
	return stream.readContext
}

func StreamReadContext[T streamable](stream StreamReaderInterface[T]) context.Context {
	if source, ok := stream.(interface{ ReadContext() context.Context }); ok {
		return source.ReadContext()
	}
	return nil
}

func (stream *streamReader[T]) CloseAndDrain() {
	_ = stream.CloseAndDrainContext(context.Background())
}

func (stream *streamReader[T]) CloseAndDrainContext(ctx context.Context) error {
	if stream == nil {
		return nil
	}
	dataChan, errChan := stream.Recv()
	stream.Close()
	for dataChan != nil || errChan != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case _, ok := <-dataChan:
			if !ok {
				dataChan = nil
			}
		case _, ok := <-errChan:
			if !ok {
				errChan = nil
			}
		}
	}
	return nil
}

func (stream *streamReader[T]) finish() {
	if stream == nil {
		return
	}
	stream.finishOnce.Do(func() {
		close(stream.DataChan)
		close(stream.ErrChan)
	})
}

func (stream *streamReader[T]) sendErr(err error) {
	sendStreamError(stream, err)
}

func streamDone[T streamable](stream *streamReader[T]) <-chan struct{} {
	if stream == nil || stream.done == nil {
		return nil
	}
	return stream.done
}

func sendStreamError[T streamable](stream *streamReader[T], err error) bool {
	if stream != nil && stream.readEnded != nil && err != nil {
		stream.readEnded(err)
	}
	if stream == nil || err == nil {
		return false
	}
	if stream.done != nil {
		select {
		case <-stream.done:
			return false
		default:
		}
	}
	timer := time.NewTimer(streamErrorSendTimeout)
	defer timer.Stop()
	select {
	case stream.ErrChan <- err:
		return true
	case <-streamDone(stream):
		return false
	case <-timer.C:
		logger.SysError(fmt.Sprintf("failed to send stream error: %v", err))
		stream.Close()
		return false
	}
}

func streamReaderPanicClass(recovered any) string {
	if recovered == nil {
		return ""
	}
	if _, ok := recovered.(runtime.Error); ok {
		return "runtime_error"
	}
	if _, ok := recovered.(error); ok {
		return "error"
	}
	if _, ok := recovered.(string); ok {
		return "string"
	}
	return "other"
}

func streamReaderStackHash(stack []byte) string {
	sum := sha256.Sum256(stack)
	return fmt.Sprintf("%x", sum[:8])
}
