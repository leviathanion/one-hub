package wstest

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"one-api/common/wsconn"
)

type Option func(*options)

type options struct {
	clock wsconn.Clock
}

func WithClock(clock wsconn.Clock) Option {
	return func(o *options) { o.clock = clock }
}

func Pair(t testing.TB, opts ...Option) (client, server *wsconn.ManagedConn) {
	t.Helper()
	accepted := make(chan *wsconn.ManagedConn, 1)
	cfg := configFromOptions(opts...)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := wsconn.AcceptManaged(w, r, cfg, wsconn.AcceptOptions{
			CheckOrigin: func(*http.Request) bool { return true },
		})
		if err != nil {
			t.Errorf("accept managed: %v", err)
			return
		}
		accepted <- conn
	}))
	t.Cleanup(ts.Close)

	url := "ws" + strings.TrimPrefix(ts.URL, "http")
	var err error
	client, err = wsconn.DialManaged(t.Context(), url, nil, cfg,
		wsconn.WithDialSecurityPolicy(wsconn.DialSecurityPolicy{
			AllowInsecureWS: true,
			AllowPrivateIP:  true,
		}),
	)
	if err != nil {
		t.Fatalf("dial managed: %v", err)
	}
	select {
	case server = <-accepted:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for accepted managed conn")
	}
	return client, server
}

func Server(t testing.TB, handler func(*wsconn.ManagedConn), opts ...Option) (url string, cleanup func()) {
	t.Helper()
	cfg := configFromOptions(opts...)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := wsconn.AcceptManaged(w, r, cfg, wsconn.AcceptOptions{
			CheckOrigin: func(*http.Request) bool { return true },
		})
		if err != nil {
			t.Errorf("accept managed: %v", err)
			return
		}
		handler(conn)
	}))
	return "ws" + strings.TrimPrefix(ts.URL, "http"), ts.Close
}

func configFromOptions(opts ...Option) wsconn.Config {
	var o options
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	return wsconn.Config{Clock: o.clock}
}
