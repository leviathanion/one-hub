package bedrock

import (
	"errors"
	"testing"

	"one-api/model"
	"one-api/providers/base"
	"one-api/types"
)

func TestBedrockFactoryAssessmentUsesRuntimeCategoryDispatch(t *testing.T) {
	factory := BedrockProviderFactory{}
	for _, modelName := range []string{"claude-sonnet-4-20250514", "us.anthropic.claude-3-5-sonnet-20241022-v2:0"} {
		if _, err := factory.AssessChatRequest(&model.Channel{}, modelName, &types.ChatCompletionRequest{}); err != nil {
			t.Fatalf("valid Bedrock model %q rejected: %v", modelName, err)
		}
	}
	_, err := factory.AssessChatRequest(&model.Channel{}, "amazon.nova-pro-v1:0", &types.ChatCompletionRequest{})
	var capabilityErr *base.RequestCapabilityError
	if !errors.As(err, &capabilityErr) || capabilityErr.Param != "model" {
		t.Fatalf("unsupported Bedrock model error=%v", err)
	}
}
