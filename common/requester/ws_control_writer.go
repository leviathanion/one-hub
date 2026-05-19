package requester

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"one-api/common/logger"

	"github.com/gorilla/websocket"
)

const (
	defaultWSControlFrameQueueSize = 8
	defaultWSControlFrameTimeout   = 2 * time.Second
)

var (
	ErrWSControlFrameWriterClosed = errors.New("websocket control frame writer is closed")
	ErrWSControlFrameQueueFull    = errors.New("websocket control frame queue is full")
)

type WSControlFrameConn interface {
	WriteControl(messageType int, data []byte, deadline time.Time) error
	Close() error
}

type WSControlFrameWriterOptions struct {
	Label        string
	LogContext   context.Context
	QueueSize    int
	WriteTimeout time.Duration
}

type wsControlFrame struct {
	messageType int
	data        []byte
}

type WSControlFrameWriter struct {
	conn         WSControlFrameConn
	label        string
	logContext   context.Context
	writeTimeout time.Duration
	queue        chan wsControlFrame
	stop         chan struct{}
	done         chan struct{}
	stopOnce     sync.Once
}

func NewWSControlFrameWriter(conn WSControlFrameConn, options WSControlFrameWriterOptions) *WSControlFrameWriter {
	if conn == nil {
		return nil
	}
	queueSize := options.QueueSize
	if queueSize <= 0 {
		queueSize = defaultWSControlFrameQueueSize
	}
	writeTimeout := options.WriteTimeout
	if writeTimeout <= 0 {
		writeTimeout = defaultWSControlFrameTimeout
	}
	logContext := options.LogContext
	if logContext == nil {
		logContext = context.Background()
	}
	writer := &WSControlFrameWriter{
		conn:         conn,
		label:        strings.TrimSpace(options.Label),
		logContext:   logContext,
		writeTimeout: writeTimeout,
		queue:        make(chan wsControlFrame, queueSize),
		stop:         make(chan struct{}),
		done:         make(chan struct{}),
	}
	go writer.run()
	return writer
}

func (w *WSControlFrameWriter) Enqueue(messageType int, data []byte) error {
	if w == nil {
		return ErrWSControlFrameWriterClosed
	}
	copied := append([]byte(nil), data...)
	frame := wsControlFrame{messageType: messageType, data: copied}
	select {
	case <-w.done:
		return ErrWSControlFrameWriterClosed
	case <-w.stop:
		return ErrWSControlFrameWriterClosed
	default:
	}
	select {
	case <-w.done:
		return ErrWSControlFrameWriterClosed
	case <-w.stop:
		return ErrWSControlFrameWriterClosed
	case w.queue <- frame:
		return nil
	default:
		return ErrWSControlFrameQueueFull
	}
}

func (w *WSControlFrameWriter) EnqueuePong(appData string) error {
	return w.Enqueue(websocket.PongMessage, []byte(appData))
}

func (w *WSControlFrameWriter) Stop() {
	if w == nil {
		return
	}
	w.stopOnce.Do(func() {
		close(w.stop)
	})
}

func (w *WSControlFrameWriter) Wait() {
	if w == nil {
		return
	}
	<-w.done
}

func (w *WSControlFrameWriter) Done() <-chan struct{} {
	if w == nil {
		return nil
	}
	return w.done
}

func (w *WSControlFrameWriter) run() {
	defer close(w.done)
	for {
		select {
		case <-w.stop:
			return
		case frame := <-w.queue:
			select {
			case <-w.stop:
				return
			default:
			}
			if err := w.conn.WriteControl(frame.messageType, frame.data, time.Now().Add(w.writeTimeout)); err != nil {
				w.logWarn(fmt.Sprintf("websocket control frame write failed: %v", err))
				_ = w.conn.Close()
				return
			}
		}
	}
}

func (w *WSControlFrameWriter) logWarn(message string) {
	label := "websocket"
	if w != nil && w.label != "" {
		label = w.label
	}
	formatted := fmt.Sprintf("%s: %s", label, message)
	if logger.Logger != nil {
		logger.LogWarn(w.logContext, formatted)
		return
	}
	log.Print(formatted)
}
