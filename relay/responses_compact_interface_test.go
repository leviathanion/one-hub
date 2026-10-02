package relay

import (
	"context"
	"github.com/gin-gonic/gin"
	"net/http"
	"net/http/httptest"
	"one-api/common/config"
	commonresponses "one-api/common/responses"
	"one-api/model"
	providersBase "one-api/providers/base"
	"one-api/types"
	"testing"
)

var _ providersBase.ResponsesCompactInterface = (*compactSuccessProvider)(nil)

type createOnlyResponsesProvider struct {
	providersBase.BaseProvider
	called bool
}

func (p *createOnlyResponsesProvider) GetRequestHeaders() map[string]string { return nil }
func (p *createOnlyResponsesProvider) CreateResponses(context.Context, *commonresponses.Request) (*types.OpenAIResponsesResponses, *types.OpenAIErrorWithStatusCode) {
	p.called = true
	return nil, nil
}
func (p *createOnlyResponsesProvider) CreateResponsesStream(context.Context, *commonresponses.Request) (commonresponses.EventStream, *types.OpenAIErrorWithStatusCode) {
	p.called = true
	return nil, nil
}

var _ providersBase.ResponsesInterface = (*createOnlyResponsesProvider)(nil)

func TestResponsesInterfacesAreOperationIndependent(t *testing.T) {
	if _, ok := any(&compactSuccessProvider{}).(providersBase.ResponsesInterface); ok {
		t.Fatal("compact-only satisfies create interface")
	}
	if _, ok := any(&createOnlyResponsesProvider{}).(providersBase.ResponsesCompactInterface); ok {
		t.Fatal("create-only satisfies compact interface")
	}
}
func TestResponsesCreateOnlyRejectsCompactBeforeProviderWork(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	enableResponsesTestDeadline(c)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/compact", nil)
	p := &createOnlyResponsesProvider{BaseProvider: providersBase.BaseProvider{Channel: &model.Channel{Type: config.ChannelTypeOpenAI}}}
	r := &relayResponses{relayBase: relayBase{c: c, provider: p, modelName: "gpt-5"}, operation: responsesOperationCompact, responsesRequest: types.OpenAIResponsesRequest{Model: "gpt-5"}, rawEnvelope: responsesTestRawEnvelope(t, types.OpenAIResponsesRequest{Model: "gpt-5"})}
	err, done := r.send()
	if err == nil || !done || err.StatusCode != http.StatusServiceUnavailable || p.called {
		t.Fatalf("done=%v err=%v called=%v", done, err, p.called)
	}
}
func TestResponsesCompactCapabilityGateRunsBeforeProviderWork(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	enableResponsesTestDeadline(c)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/compact", nil)
	p := &compactRejectProvider{BaseProvider: providersBase.BaseProvider{Channel: &model.Channel{Type: config.ChannelTypeCohere}}}
	r := &relayResponses{relayBase: relayBase{c: c, provider: p}, operation: responsesOperationCompact}
	if err := r.validateSelectedProviderRequest(); err == nil || p.compactCalled {
		t.Fatalf("err=%v called=%v", err, p.compactCalled)
	}
}
