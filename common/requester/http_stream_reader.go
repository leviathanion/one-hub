package requester

import (
	"bufio"
	"bytes"
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

type streamReader[T streamable] struct {
	reader   *bufio.Reader
	response *http.Response
	NoTrim   bool
	options  StreamReadOptions

	handlerPrefix        HandlerPrefix[T]
	handlerPrefixEmitter HandlerPrefixWithEmitter[T]

	DataChan   chan T
	ErrChan    chan error
	done       chan struct{}
	closeOnce  sync.Once
	recvOnce   sync.Once
	finishOnce sync.Once
}

type StreamReadOptions struct {
	MaxLineBytes int64
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
		if readErr != nil {
			if errors.Is(readErr, ErrStreamLineTooLarge) {
				if stream.response != nil && stream.response.Body != nil {
					_ = stream.response.Body.Close()
				}
			}
			stream.sendErr(readErr)
			if errors.Is(readErr, ErrStreamLineTooLarge) {
				stream.Close()
			}
			return
		}

		if !stream.NoTrim {
			rawLine = bytes.TrimSpace(rawLine)
			if len(rawLine) == 0 {
				continue
			}
		}

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

		if rawLine == nil {
			continue
		}

		if bytes.Equal(rawLine, StreamClosed) {
			return
		}
	}
}

func (stream *streamReader[T]) readLine() ([]byte, error) {
	if stream == nil || stream.reader == nil {
		return nil, io.ErrClosedPipe
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
		return nil, err
	}
}

func (stream *streamReader[T]) Close() {
	if stream == nil {
		return
	}
	stream.closeOnce.Do(func() {
		if stream.done != nil {
			close(stream.done)
		}
		if stream.response != nil && stream.response.Body != nil {
			_ = stream.response.Body.Close()
		}
	})
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
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case stream.ErrChan <- err:
		return true
	case <-streamDone(stream):
		return false
	case <-timer.C:
		logger.SysError(fmt.Sprintf("无法发送流错误: %v", err))
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
