package responses

import (
	"strings"
	"testing"

	"one-api/types"
)

func TestProjectRequestAppliesControlPolicyAndModelNormalizer(t *testing.T) {
	req := &Request{
		Body: &RawEnvelope{Projection: types.OpenAIResponsesRequest{
			Model: " body-model ",
		}},
		Control: Control{
			DownstreamDialect: DownstreamChatCompletions,
			Stream:            true,
		},
		Policy: PolicyInput{
			PromptCache: &PromptCacheDecision{Key: " policy-cache "},
		},
		Model: " route-model ",
	}

	projected := ProjectRequest(req, func(model string) string {
		return strings.ToUpper(strings.TrimSpace(model))
	})
	if projected.Model != "ROUTE-MODEL" || !projected.Stream || !projected.ConvertChat || projected.PromptCacheKey != "policy-cache" {
		t.Fatalf("unexpected projection: %+v", projected)
	}

	req.Model = ""
	req.Body.Projection.PromptCacheKey = "client-cache"
	projected = ProjectRequest(req, nil)
	if projected.Model != "body-model" || projected.PromptCacheKey != "client-cache" {
		t.Fatalf("expected body model and client prompt cache to win, got %+v", projected)
	}
}
