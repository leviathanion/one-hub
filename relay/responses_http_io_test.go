package relay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"one-api/common/config"
	commonresponses "one-api/common/responses"
	"one-api/model"
	providersBase "one-api/providers/base"
	"one-api/types"
)

// 组件测试的内存 writer 没有 socket；真实 deadline/abort 由连接集成测试验证。
type responsesTestDeadlineWriter struct{ gin.ResponseWriter }

func (*responsesTestDeadlineWriter) SetWriteDeadline(time.Time) error { return nil }
func (w *responsesTestDeadlineWriter) FlushError() error              { w.ResponseWriter.Flush(); return nil }
func enableResponsesTestDeadline(c *gin.Context)                      { c.Writer = &responsesTestDeadlineWriter{c.Writer} }

type responsesDeadlineProbeProvider struct {
	streamAffinityResponsesProvider
	called bool
}

func (p *responsesDeadlineProbeProvider) CreateResponsesStream(context.Context, *commonresponses.Request) (commonresponses.EventStream, *types.OpenAIErrorWithStatusCode) {
	p.called = true
	return nil, nil
}

func TestResponsesHTTPRejectsUnsupportedWriteDeadlineBeforeProviderWork(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	provider := &responsesDeadlineProbeProvider{streamAffinityResponsesProvider: streamAffinityResponsesProvider{
		BaseProvider: providersBase.BaseProvider{Channel: &model.Channel{Id: 17, Type: config.ChannelTypeOpenAI}},
	}}
	envelope, err := commonresponses.ParseRawEnvelope([]byte(`{"model":"gpt-test","input":"hello","stream":true,"store":false}`))
	if err != nil {
		t.Fatal(err)
	}
	r := &relayResponses{
		relayBase:        relayBase{c: c, provider: provider, modelName: "gpt-test"},
		responsesRequest: envelope.Projection, rawEnvelope: envelope, operation: responsesOperationCreate,
	}
	apiErr, done := r.sendCurrentProvider()
	if !done || apiErr == nil || apiErr.Code != "response_write_deadline_unsupported" || !apiErr.LocalError {
		t.Fatalf("missing local deadline capability rejection: done=%t error=%+v", done, apiErr)
	}
	if provider.called || c.Writer.Written() {
		t.Fatalf("unsupported writer reached provider or committed downstream: called=%t written=%t", provider.called, c.Writer.Written())
	}
}
