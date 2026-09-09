package responses

import (
	"strings"
	"testing"

	"one-api/types"
)

func TestParseRawEnvelopeKeepsProviderOwnedFutureUnionRaw(t *testing.T) {
	raw := []byte(`{"model":"gpt-5","input":"hi","stream":true,"background":true,"conversation":{"id":"conv_1"},"prompt":{"id":"pmpt_1"},"tools":[{"type":"future_tool","max_num_results":"future-shape"}],"future":{"keep":true}}`)
	envelope, err := ParseRawEnvelope(raw)
	if err != nil {
		t.Fatalf("parse raw envelope: %v", err)
	}
	if envelope.ProjectionError == nil {
		t.Fatal("expected the closed DTO projection to report an incompatibility")
	}
	if envelope.Projection.Model != "gpt-5" || envelope.Projection.Input != "hi" || !envelope.Projection.Stream ||
		envelope.Projection.Background == nil || !*envelope.Projection.Background || envelope.Projection.Conversation == nil || envelope.Projection.Prompt == nil {
		t.Fatalf("expected proxy-owned core fields to remain available, got %+v", envelope.Projection)
	}
	if got := string(envelope.Object.Fields["tools"]); !strings.Contains(got, `"future-shape"`) {
		t.Fatalf("expected provider-owned union to remain raw, got %s", got)
	}
}

func TestParseRawEnvelopeRejectsInvalidProxyOwnedCoreField(t *testing.T) {
	if _, err := ParseRawEnvelope([]byte(`{"model":42,"input":"hi"}`)); err == nil {
		t.Fatal("expected non-string model to fail the proxy envelope projection")
	}
}

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
