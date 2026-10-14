package antigravity

import (
	"encoding/json"
	"slices"
	"testing"

	"one-api/common/config"
	"one-api/providers/antigravity/wire"
)

func TestToolResultImagesRequireProviderInputImageEvidence(t *testing.T) {
	for _, test := range []struct {
		dialect wire.Dialect
		input   string
	}{
		{wire.DialectChat, `{"messages":[{"role":"assistant","tool_calls":[{"id":"call_a","type":"function","function":{"name":"inspect","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_a","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,aW1hZ2U="}}]}]}`},
		{wire.DialectClaude, `{"max_tokens":32,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call_a","name":"inspect","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_a","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aW1hZ2U="}}]}]}]}`},
		{wire.DialectResponses, `{"store":false,"input":[{"type":"function_call","call_id":"call_a","name":"inspect","arguments":"{}"},{"type":"function_call_output","call_id":"call_a","output":[{"type":"input_image","image_url":"data:image/png;base64,aW1hZ2U="}]}]}`},
		{wire.DialectGemini, `{"contents":[{"role":"user","parts":[{"functionResponse":{"name":"inspect","response":{},"parts":[{"inlineData":{"mimeType":"image/png","data":"aW1hZ2U="}}]}}]}]}`},
	} {
		t.Run(string(test.dialect), func(t *testing.T) {
			request, err := convertedRequest(nil, test.dialect, []byte(test.input), "model")
			if err != nil {
				t.Fatal(err)
			}
			provider := &AntigravityProvider{}
			provider.usageObserver(request)(json.RawMessage(`{"promptTokenCount":5,"candidatesTokenCount":0,"totalTokenCount":5}`), "")
			usage := provider.Usage
			if usage == nil || !usage.HasProviderBaseUsage() || usage.HasProviderUsage() || !slices.Contains(usage.RequiredTokenExtraKeys, config.UsageExtraInputImageTokens) {
				t.Fatalf("工具图片未要求上游提供输入图片分区: %+v", usage)
			}
		})
	}
}
