package zhipu

import (
	"testing"

	"gorm.io/datatypes"

	"one-api/model"
	"one-api/types"
)

func TestZhipuSearchIsRejectedBeforeProviderWork(t *testing.T) {
	for _, toolType := range []string{"web_search", "web_browser"} {
		request := &types.ChatCompletionRequest{Tools: []*types.ChatCompletionTool{{Type: toolType}}}
		if _, err := (ZhipuProviderFactory{}).AssessChatRequest(nil, "glm-4", request); err == nil {
			t.Fatalf("expected %s to be rejected by evidence gate", toolType)
		}
	}
	request := &types.ChatCompletionRequest{Tools: []*types.ChatCompletionTool{{Type: "function"}}}
	if _, err := (ZhipuProviderFactory{}).AssessChatRequest(nil, "glm-4", request); err != nil {
		t.Fatalf("ordinary function tool was rejected: %+v", err)
	}
	plugin := datatypes.NewJSONType(model.PluginType{"web_search": {"enable": true}})
	if _, err := (ZhipuProviderFactory{}).AssessChatRequest(&model.Channel{Plugin: &plugin}, "glm-4", &types.ChatCompletionRequest{}); err == nil {
		t.Fatal("factory missed plugin-injected Zhipu search")
	}
}
