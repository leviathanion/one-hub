package requester

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"one-api/common"
	"one-api/types"

	"github.com/gorilla/websocket"
)

const wsDialBodySnippetLimit = 4 << 10

type WSRequester struct {
	WSClient *websocket.Dialer
}

type WSDialHandshakeError struct {
	URL           string
	StatusCode    int
	Header        http.Header
	BodySnippet   []byte
	BodyTruncated bool
	BodyReadErr   error
	Err           error
}

func (e *WSDialHandshakeError) Error() string {
	if e == nil {
		return ""
	}
	if e.StatusCode > 0 {
		return fmt.Sprintf("websocket handshake failed: status %d: %v", e.StatusCode, e.Err)
	}
	return fmt.Sprintf("websocket dial failed: %v", e.Err)
}

func (e *WSDialHandshakeError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func NewWSRequester(proxyAddr string) *WSRequester {
	return &WSRequester{
		WSClient: GetWSClient(proxyAddr),
	}
}

func (w *WSRequester) NewRequest(url string, header http.Header) (*websocket.Conn, error) {
	return w.NewRequestContext(context.Background(), url, header)
}

func (w *WSRequester) NewRequestContext(ctx context.Context, url string, header http.Header) (*websocket.Conn, error) {
	return w.NewRequestContextWithSubprotocols(ctx, url, header, nil)
}

func (w *WSRequester) NewRequestContextWithSubprotocols(ctx context.Context, url string, header http.Header, subprotocols []string) (*websocket.Conn, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	dialer := w.WSClient
	if len(subprotocols) > 0 && dialer != nil {
		cloned := *dialer
		cloned.Subprotocols = append([]string(nil), subprotocols...)
		dialer = &cloned
	}
	if dialer == nil {
		dialer = websocket.DefaultDialer
	}
	conn, resp, err := dialer.DialContext(ctx, url, header)
	if err != nil {
		return nil, newWSDialHandshakeError(url, resp, err)
	}

	if resp.StatusCode != http.StatusSwitchingProtocols {
		if conn != nil {
			_ = conn.Close()
		}
		return nil, newWSDialHandshakeError(url, resp, errors.New("ws unexpected status code"))
	}

	return conn, nil
}

func newWSDialHandshakeError(rawURL string, resp *http.Response, err error) error {
	if resp == nil {
		return err
	}
	handshakeErr := &WSDialHandshakeError{
		URL:        rawURL,
		StatusCode: resp.StatusCode,
		Header:     resp.Header.Clone(),
		Err:        err,
	}
	if resp.Body != nil {
		limited := io.LimitReader(resp.Body, wsDialBodySnippetLimit+1)
		body, readErr := io.ReadAll(limited)
		_ = resp.Body.Close()
		if readErr != nil {
			handshakeErr.BodyReadErr = readErr
		} else {
			if len(body) > wsDialBodySnippetLimit {
				handshakeErr.BodySnippet = append([]byte(nil), body[:wsDialBodySnippetLimit]...)
				handshakeErr.BodyTruncated = true
			} else {
				handshakeErr.BodySnippet = append([]byte(nil), body...)
			}
		}
	}
	return handshakeErr
}

func SendWSJsonRequest[T streamable](conn *websocket.Conn, data any, handlerPrefix HandlerPrefix[T]) (*wsReader[T], *types.OpenAIErrorWithStatusCode) {
	err := conn.WriteJSON(data)
	if err != nil {
		return nil, common.ErrorWrapper(err, "ws_request_failed", http.StatusInternalServerError)
	}

	stream := &wsReader[T]{
		reader:        conn,
		handlerPrefix: handlerPrefix,

		DataChan: make(chan T, 1),
		ErrChan:  make(chan error, 1),
	}

	return stream, nil
}

// 设置请求头
func (w *WSRequester) WithHeader(headers map[string]string) http.Header {
	header := make(http.Header)
	for k, v := range headers {
		header.Set(k, v)
	}
	return header
}
