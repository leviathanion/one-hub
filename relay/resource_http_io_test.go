package relay

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// net.Pipe supplies actual interruptible I/O, unlike a recorder that merely
// accepts deadline calls. The peer intentionally stops consuming/producing.
type resourceSocketDeadlineWriter struct {
	gin.ResponseWriter
	conn    net.Conn
	started chan struct{}
	once    sync.Once
	reads   atomic.Int32
}

func (w *resourceSocketDeadlineWriter) SetWriteDeadline(deadline time.Time) error {
	return w.conn.SetWriteDeadline(deadline)
}
func (w *resourceSocketDeadlineWriter) SetReadDeadline(deadline time.Time) error {
	w.reads.Add(1)
	return w.conn.SetReadDeadline(deadline)
}
func (w *resourceSocketDeadlineWriter) Write(p []byte) (int, error) {
	if w.started != nil {
		w.once.Do(func() { close(w.started) })
	}
	return w.conn.Write(p)
}
func (w *resourceSocketDeadlineWriter) WriteString(s string) (int, error) { return w.Write([]byte(s)) }
func (w *resourceSocketDeadlineWriter) FlushError() error                 { return nil }

type resourceWriteOnlyDeadlineWriter struct{ gin.ResponseWriter }

func (*resourceWriteOnlyDeadlineWriter) SetWriteDeadline(time.Time) error { return nil }

func resourceHandlerExit(c *gin.Context) <-chan any {
	result := make(chan any, 1)
	go func() {
		defer func() { result <- recover() }()
		ResourceRelay(c)
	}()
	return result
}
func awaitResourceAbort(t *testing.T, result <-chan any) {
	t.Helper()
	select {
	case recovered := <-result:
		if recovered != http.ErrAbortHandler {
			t.Fatalf("expected transport abort, got %v", recovered)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("resource I/O did not stop within the transport deadline")
	}
}

func TestResourceHTTPIOUnsupportedDeadlineRejectsBeforeUpstream(t *testing.T) {
	for _, readUnsupported := range []bool{false, true} {
		t.Run(map[bool]string{false: "write", true: "read"}[readUnsupported], func(t *testing.T) {
			_, calls := setupResourceHTTPTest(t, func(w http.ResponseWriter, r *http.Request) { t.Error("unsupported local I/O reached provider") })
			c, rec := resourceHTTPContext("POST", "/v1/files", strings.NewReader("raw multipart"))
			// Remove the recorder deadline stub to exercise real capability detection.
			original := c.Writer.(*responsesTestDeadlineWriter).ResponseWriter
			c.Writer = original
			if readUnsupported {
				c.Writer = &resourceWriteOnlyDeadlineWriter{ResponseWriter: original}
			}
			ResourceRelay(c)
			if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "resource_io_deadline_unsupported") || calls.Load() != 0 {
				t.Fatalf("response=%d %s calls=%d", rec.Code, rec.Body, calls.Load())
			}
		})
	}
}

func TestResourceHTTPIOSlowDownloadAndCancellationStopOneRequest(t *testing.T) {
	for _, cancelOnWrite := range []bool{false, true} {
		t.Run(map[bool]string{false: "deadline", true: "cancellation"}[cancelOnWrite], func(t *testing.T) {
			channel, calls := setupResourceHTTPTest(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/octet-stream")
				io.WriteString(w, strings.Repeat("raw", 32<<10))
			})
			seedResourceHTTPOwner(t, channel, "file", "file_slow", 101)
			c, _ := resourceHTTPContext("GET", "/v1/files/file_slow/content", nil)
			parent, cancel := context.WithTimeout(c.Request.Context(), 250*time.Millisecond)
			defer cancel()
			if cancelOnWrite {
				parent, cancel = context.WithCancel(c.Request.Context())
				defer cancel()
			}
			c.Request = c.Request.WithContext(parent)
			server, client := net.Pipe()
			defer server.Close()
			defer client.Close()
			writer := &resourceSocketDeadlineWriter{ResponseWriter: c.Writer, conn: server, started: make(chan struct{})}
			c.Writer = writer
			result := resourceHandlerExit(c)
			if cancelOnWrite {
				select {
				case <-writer.started:
					cancel()
				case <-time.After(2 * time.Second):
					t.Fatal("download never reached the blocked client write")
				}
			}
			awaitResourceAbort(t, result)
			if calls.Load() != 1 {
				t.Fatalf("download was retried or never submitted: %d", calls.Load())
			}
		})
	}
}

func TestResourceHTTPIOSlowUploadHasActualReadDeadline(t *testing.T) {
	_, calls := setupResourceHTTPTest(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"incomplete upload"}}`)
	})
	c, _ := resourceHTTPContext("POST", "/v1/files", nil)
	parent, cancel := context.WithTimeout(c.Request.Context(), 250*time.Millisecond)
	defer cancel()
	c.Request = c.Request.WithContext(parent)
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	c.Request.Body = io.NopCloser(server)
	c.Request.ContentLength = 1 << 20
	c.Request.Header.Set("Content-Type", "multipart/form-data; boundary=raw")
	writer := &resourceSocketDeadlineWriter{ResponseWriter: c.Writer, conn: server}
	c.Writer = writer
	sent := make(chan struct{})
	go func() { _, _ = client.Write(make([]byte, 64<<10)); close(sent) }()
	result := resourceHandlerExit(c)
	awaitResourceAbort(t, result)
	if writer.reads.Load() < 2 {
		t.Fatalf("upload did not arm a real read deadline: %d", writer.reads.Load())
	}
	if calls.Load() > 1 {
		t.Fatalf("ambiguous upload retried: %d", calls.Load())
	}
	client.Close()
	select {
	case <-sent:
	case <-time.After(time.Second):
		t.Fatal("fixture upload goroutine was not stopped")
	}
}

func TestResourceHTTPIOIdleWriteDeadlineDoesNotNeedBusinessTerminal(t *testing.T) {
	c, _ := resourceHTTPContext("GET", "/v1/files/file/content", nil)
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	c.Writer = &resourceSocketDeadlineWriter{ResponseWriter: c.Writer, conn: server}
	owner, err := beginResourceHTTPIO(c)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	owner.idle = 25 * time.Millisecond
	start := time.Now()
	_, err = c.Writer.Write([]byte("safe raw bytes"))
	var timeout net.Error
	if err == nil {
		t.Fatal("slow socket write unexpectedly completed")
	}
	if !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatalf("expected actual socket timeout, got %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("idle write deadline was not applied")
	}
}

func TestResourceHTTPIOIdleReadDeadlineStopsActualSocketRead(t *testing.T) {
	c, _ := resourceHTTPContext("POST", "/v1/files", nil)
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	c.Request.Body = io.NopCloser(server)
	c.Writer = &resourceSocketDeadlineWriter{ResponseWriter: c.Writer, conn: server}
	owner, err := beginResourceHTTPIO(c)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	owner.idle = 25 * time.Millisecond
	_, err = c.Request.Body.Read(make([]byte, 32))
	var timeout net.Error
	if !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatalf("expected socket read timeout, got %v", err)
	}
}

func TestResourceHTTPIOFinalizationKeepsSocketBounds(t *testing.T) {
	c, _ := resourceHTTPContext("POST", "/v1/files", nil)
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	c.Request.Body = io.NopCloser(server)
	c.Writer = &resourceSocketDeadlineWriter{ResponseWriter: c.Writer, conn: server}
	owner, err := beginResourceHTTPIO(c)
	if err != nil {
		t.Fatal(err)
	}
	owner.idle = 25 * time.Millisecond
	if err := owner.armRead(); err != nil {
		t.Fatal(err)
	}
	if err := owner.arm(); err != nil {
		t.Fatal(err)
	}
	owner.Close()
	// net/http does these final framing/draining writes after ServeHTTP returns.
	_, err = server.Write([]byte("final HTTP framing"))
	var timeout net.Error
	if !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatalf("final server write lost its deadline: %v", err)
	}
	_, err = server.Read(make([]byte, 1))
	if !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatalf("final server body drain lost its deadline: %v", err)
	}
}

func TestResourceHTTPIORealHTTPKeepAliveRestoresServerDeadlines(t *testing.T) {
	channel, calls := setupResourceHTTPTest(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "binary") })
	seedResourceHTTPOwner(t, channel, "file", "file_socket", 101)
	engine := gin.New()
	engine.GET("/v1/files/file_socket/content", func(c *gin.Context) {
		c.Set("id", 101)
		c.Set("token_id", 201)
		ctx, cancel := context.WithTimeout(c.Request.Context(), 100*time.Millisecond)
		defer cancel()
		c.Request = c.Request.WithContext(ctx)
		ResourceRelay(c)
	})
	engine.GET("/probe", func(c *gin.Context) { time.Sleep(150 * time.Millisecond); c.String(200, "later") })
	server := httptest.NewServer(engine)
	defer server.Close()
	client := server.Client()
	client.Timeout = 2 * time.Second
	response, err := client.Get(server.URL + "/v1/files/file_socket/content")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != 200 || string(raw) != "binary" {
		t.Fatalf("resource response=%d %q %v", response.StatusCode, raw, err)
	}
	var reused bool
	request, _ := http.NewRequest("GET", server.URL+"/probe", nil)
	request = request.WithContext(httptrace.WithClientTrace(request.Context(), &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused }}))
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	raw, err = io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != 200 || string(raw) != "later" || !reused || calls.Load() != 1 {
		t.Fatalf("next response=%d %q err=%v reused=%v upstream=%d", response.StatusCode, raw, err, reused, calls.Load())
	}
}
