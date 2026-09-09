package vertexai

import (
	"errors"
	"testing"

	"one-api/model"
	"one-api/providers/base"
	"one-api/types"
)

func TestVertexFactoryAssessmentUsesRuntimeCategoryDispatch(t *testing.T) {
	factory := VertexAIProviderFactory{}
	for _, modelName := range []string{"gemini-2.5-pro", "claude-sonnet-4"} {
		if _, err := factory.AssessChatRequest(&model.Channel{}, modelName, &types.ChatCompletionRequest{}); err != nil {
			t.Fatalf("valid Vertex model %q rejected: %v", modelName, err)
		}
	}
	for _, modelName := range []string{"imagen-3", "Gemini-2.5-pro"} {
		_, err := factory.AssessChatRequest(&model.Channel{}, modelName, &types.ChatCompletionRequest{})
		var capabilityErr *base.RequestCapabilityError
		if !errors.As(err, &capabilityErr) || capabilityErr.Param != "model" {
			t.Fatalf("invalid Vertex model %q error=%v", modelName, err)
		}
	}
	_, err := factory.AssessChatRequest(&model.Channel{}, "gemini-2.5-pro", &types.ChatCompletionRequest{ServiceTier: "flex"})
	var capabilityErr *base.RequestCapabilityError
	if !errors.As(err, &capabilityErr) || capabilityErr.Param != "service_tier" {
		t.Fatalf("Vertex Gemini missed native converter boundary: %v", err)
	}
}
