package relay

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"one-api/common/logger"
	"one-api/common/requester"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

type fakeRelayStream struct {
	dataChan chan string
	errChan  chan error
}

var _ requester.StreamReaderInterface[string] = (*fakeRelayStream)(nil)

func (s *fakeRelayStream) Recv() (<-chan string, <-chan error) {
	return s.dataChan, s.errChan
}

func (s *fakeRelayStream) Close() {}

func TestResponseStreamClientDoesNotReturnMidStreamError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger.Logger = zap.NewNop()

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/", nil)

	stream := &fakeRelayStream{
		dataChan: make(chan string),
		errChan:  make(chan error),
	}

	go func() {
		stream.dataChan <- `{"id":"chunk-1"}`
		stream.errChan <- errors.New("upstream stream broken")
	}()

	firstResponseTime, errWithCode := responseStreamClient(ctx, stream, nil)
	if errWithCode != nil {
		t.Fatalf("expected nil error, got: %v", errWithCode.Message)
	}

	if firstResponseTime.IsZero() {
		t.Fatalf("expected first response time to be set")
	}

	body := recorder.Body.String()
	if !strings.Contains(body, `data: {"id":"chunk-1"}`) {
		t.Fatalf("expected stream body to include first chunk, got: %q", body)
	}

	if !strings.Contains(body, `"stream_error"`) {
		t.Fatalf("expected stream body to include SSE error payload, got: %q", body)
	}
}
