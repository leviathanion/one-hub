package relay

import (
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
)

// beginResourceHTTPIO 只限制真实传输等待。原始 multipart、JSON、二进制
// 不经重建；权限与资源提交仍由调用方负责，不观察上游业务状态。
func beginResourceHTTPIO(c *gin.Context) (*responsesHTTPIO, error) {
	hasBody := c.Request.Body != nil && c.Request.Body != http.NoBody
	owner, err := newResponsesHTTPIOWithRead(c, hasBody)
	if err != nil {
		return nil, err
	}
	// net/http drains unread request bytes and emits final HTTP framing after
	// this handler returns. Keep both bounds through that work; the server
	// resets connection deadlines before serving the next request/stream.
	owner.keepDeadlines = true
	c.Request = c.Request.WithContext(owner.ctx)
	c.Writer = &resourceDeadlineWriter{ResponseWriter: c.Writer, owner: owner}
	if hasBody {
		c.Request.Body = &resourceDeadlineBody{ReadCloser: c.Request.Body, owner: owner}
	}
	return owner, nil
}

type resourceDeadlineWriter struct {
	gin.ResponseWriter
	owner *responsesHTTPIO
}

func (w *resourceDeadlineWriter) WriteHeader(code int) {
	if err := w.owner.arm(); err != nil {
		panic(http.ErrAbortHandler)
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *resourceDeadlineWriter) Write(p []byte) (int, error) {
	if err := w.owner.arm(); err != nil {
		return 0, err
	}
	n, err := w.ResponseWriter.Write(p)
	if err == nil {
		err = w.flush()
	}
	if err != nil {
		w.owner.Stop()
	}
	return n, err
}

func (w *resourceDeadlineWriter) WriteString(s string) (int, error) {
	if err := w.owner.arm(); err != nil {
		return 0, err
	}
	n, err := w.ResponseWriter.WriteString(s)
	if err == nil {
		err = w.flush()
	}
	if err != nil {
		w.owner.Stop()
	}
	return n, err
}

func (w *resourceDeadlineWriter) WriteHeaderNow() {
	if err := w.flush(); err != nil {
		panic(http.ErrAbortHandler)
	}
}

func (w *resourceDeadlineWriter) Flush() {
	if err := w.flush(); err != nil {
		panic(http.ErrAbortHandler)
	}
}

func (w *resourceDeadlineWriter) FlushError() error { return w.flush() }

// Flush while the deadline is armed, including empty responses. Otherwise
// net/http could flush its final buffered bytes only after Close cleared it.
func (w *resourceDeadlineWriter) flush() error {
	if err := w.owner.arm(); err != nil {
		return err
	}
	w.ResponseWriter.WriteHeaderNow()
	if err := w.owner.controller.Flush(); err != nil {
		w.owner.Stop()
		return err
	}
	return nil
}

type resourceDeadlineBody struct {
	io.ReadCloser
	owner *responsesHTTPIO
}

func (body *resourceDeadlineBody) Read(p []byte) (int, error) {
	if err := body.owner.armRead(); err != nil {
		return 0, err
	}
	n, err := body.ReadCloser.Read(p)
	if err != nil && err != io.EOF {
		body.owner.Stop()
	}
	return n, err
}
