package gemini

import (
	"errors"
	"testing"

	"gorm.io/datatypes"

	"one-api/model"
	"one-api/providers/base"
	"one-api/types"
)

func TestGeminiFactoryAssessmentMatchesNativeConverterBoundaries(t *testing.T) {
	factory := GeminiProviderFactory{}
	tests := []struct {
		name    string
		model   string
		request *types.ChatCompletionRequest
		param   string
	}{
		{name: "service tier", model: "gemini-3", request: &types.ChatCompletionRequest{ServiceTier: "flex"}, param: "service_tier"},
		{name: "tts model", model: "gemini-3-tts", request: &types.ChatCompletionRequest{}, param: "audio"},
		{name: "grounding function", model: "gemini-3", request: &types.ChatCompletionRequest{Tools: []*types.ChatCompletionTool{{Type: types.ToolChoiceTypeFunction, Function: types.ChatCompletionFunction{Name: "googleSearch"}}}}, param: "tools"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := factory.AssessChatRequest(&model.Channel{}, test.model, test.request)
			var capabilityErr *base.RequestCapabilityError
			if !errors.As(err, &capabilityErr) || capabilityErr.Param != test.param {
				t.Fatalf("assessment error=%v, want param %q", err, test.param)
			}
		})
	}

	plugin := datatypes.NewJSONType(model.PluginType{"use_openai_api": {"enable": true}})
	request := &types.ChatCompletionRequest{ServiceTier: "flex", Tools: []*types.ChatCompletionTool{{Type: types.ToolChoiceTypeFunction, Function: types.ChatCompletionFunction{Name: "googleSearch"}}}}
	if _, err := factory.AssessChatRequest(&model.Channel{Plugin: &plugin}, "gemini-3-tts", request); err != nil {
		t.Fatalf("OpenAI-dialect Gemini request inherited native-only restrictions: %v", err)
	}
}
