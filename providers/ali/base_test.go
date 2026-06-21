package ali

import (
	"testing"

	"one-api/model"
	"one-api/providers/base"
	"one-api/providers/openai"
)

func TestAliRequestHeadersReadDashScopePluginFromJSONOther(t *testing.T) {
	provider := &AliProvider{
		OpenAIProvider: openai.OpenAIProvider{
			BaseProvider: base.BaseProvider{
				Channel: &model.Channel{
					Key:   "sk-test",
					Other: `{"dashscope_plugin":"plugin-a"}`,
				},
			},
		},
	}

	headers := provider.GetRequestHeaders()
	if headers["X-DashScope-Plugin"] != "plugin-a" {
		t.Fatalf("expected DashScope plugin header from JSON Other, got %#v", headers)
	}
}
