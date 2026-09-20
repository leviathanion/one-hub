package openai

import (
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"one-api/common/config"
	"one-api/common/requestctx"
	"one-api/model"
)

func TestResponsesEffectiveFunctionOutputAuthorization(t *testing.T) {
	for _, kind := range []string{"input_file", "input_image"} {
		t.Run(kind, func(t *testing.T) {
			proxy := ""
			custom := `{"overwrite":true,"input":[{"type":"function_call_output","call_id":"call_1","output":[{"type":"` + kind + `","file_id":"file_injected"}]}]}`
			provider := CreateOpenAIProvider(&model.Channel{
				Type: config.ChannelTypeOpenAI, Key: "test", Proxy: &proxy, CustomParameter: &custom,
			}, "https://api.openai.com")
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
			policy := &recordingMediaPolicy{err: errors.New("resource owner denied")}
			requestctx.SetResourceReferencePolicy(c, policy)
			provider.SetContext(c)
			raw := openAIResponsesRawRequestForTest(t, `{"model":"gpt-5","input":"hello"}`, "gpt-5", false, "")
			request, apiErr := provider.buildResponsesCreateRequest(raw, responsesRequestProjection(raw), false)
			if request != nil || apiErr == nil || apiErr.Code != "unsupported_resource_reference" {
				t.Fatalf("effective resource authorization skipped: request=%v err=%v", request, apiErr)
			}
			if len(policy.refs) != 1 || policy.refs[0] != "file:file_injected" {
				t.Fatalf("wrong semantic reference: %v", policy.refs)
			}
		})
	}
}
