package wsconn

import (
	"net/http"

	"github.com/gorilla/websocket"
)

type AcceptOptions struct {
	CheckOrigin       func(*http.Request) bool
	ResponseHeader    http.Header
	ReadBufferSize    int
	WriteBufferSize   int
	EnableCompression bool
	Subprotocols      []string
	Error             func(w http.ResponseWriter, r *http.Request, status int, reason error)
}

func AcceptManaged(w http.ResponseWriter, r *http.Request, cfg Config, opts AcceptOptions) (*ManagedConn, error) {
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	upgrader := acceptUpgrader(opts)
	conn, err := upgrader.Upgrade(w, r, opts.ResponseHeader)
	if err != nil {
		return nil, err
	}
	return newManagedConn(conn, cfg), nil
}

func acceptUpgrader(opts AcceptOptions) websocket.Upgrader {
	return websocket.Upgrader{
		CheckOrigin:       opts.CheckOrigin,
		ReadBufferSize:    opts.ReadBufferSize,
		WriteBufferSize:   opts.WriteBufferSize,
		EnableCompression: opts.EnableCompression,
		Subprotocols:      append([]string(nil), opts.Subprotocols...),
		Error:             opts.Error,
	}
}
