package relay

import (
	"github.com/gin-gonic/gin"
	"net/http"
	"net/http/httptest"
	"one-api/common/config"
	"one-api/model"
	providersBase "one-api/providers/base"
	"one-api/types"
	"testing"
)

func TestCompactCrossProtocolFenceBeforeProviderWork(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	enableResponsesTestDeadline(c)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/compact", nil)
	p := &compactRejectProvider{BaseProvider: providersBase.BaseProvider{Channel: &model.Channel{Type: config.ChannelTypeOpenAI}}}
	req := types.OpenAIResponsesRequest{Model: "gpt-5"}
	r := &relayResponses{relayBase: relayBase{c: c, provider: p, modelName: "gpt-5"}, operation: responsesOperationCompact, responsesRequest: req, rawEnvelope: responsesTestRawEnvelope(t, req), selectedDataPath: providersBase.DataPathCrossProtocol}
	err, done := r.send()
	if err == nil || !done || err.StatusCode != http.StatusServiceUnavailable || p.compactCalled {
		t.Fatalf("cross protocol fence err=%v done=%v called=%v", err, done, p.compactCalled)
	}
}

func TestCompactOperationDoesNotBorrowCreateCapability(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	enableResponsesTestDeadline(c)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/compact", nil)
	p := &compactRejectProvider{BaseProvider: providersBase.BaseProvider{Channel: &model.Channel{Type: config.ChannelTypeAnthropic, CompatibleResponse: true}}}
	r := &relayResponses{relayBase: relayBase{c: c, provider: p}, operation: responsesOperationCompact}
	if operation := r.providerOperation(); operation != providersBase.OperationResponsesCompact {
		t.Fatalf("compact mapped to %s", operation)
	}
	if err := r.validateSelectedProviderRequest(); err == nil || p.compactCalled {
		t.Fatalf("compact borrowed create support err=%v called=%v", err, p.compactCalled)
	}
}
