package cohere

import (
	"encoding/json"
	"testing"

	"one-api/providers/base"
	"one-api/types"
)

func TestCohereChatKeepsTokensAndIndependentUnitsSeparate(t *testing.T) {
	var billed UsageBilledUnits
	if err := json.Unmarshal([]byte(`{"input_tokens":5,"output_tokens":2,"search_units":3,"classifications":4}`), &billed); err != nil {
		t.Fatalf("decode Cohere billed units: %v", err)
	}
	usage := usageHandle(&billed)
	if !usage.HasProviderUsage() || usage.PromptTokens != 5 || usage.CompletionTokens != 2 || usage.TotalTokens != 7 {
		t.Fatalf("Cohere token usage was distorted: %+v", usage)
	}
	for key, count := range map[string]int{"cohere_search_unit": 3, "cohere_classification_unit": 4} {
		billingKey := types.BuildExtraBillingKey(key, "")
		if usage.ExtraBilling[billingKey].CallCount != count || !usage.HasProviderExtraBilling(billingKey) {
			t.Fatalf("Cohere independent unit %s was lost: %+v", key, usage)
		}
	}
}

func TestCohereChatDoesNotAuthorizePartialTokenUnits(t *testing.T) {
	var billed UsageBilledUnits
	if err := json.Unmarshal([]byte(`{"input_tokens":5,"search_units":1}`), &billed); err != nil {
		t.Fatalf("decode Cohere billed units: %v", err)
	}
	if usage := usageHandle(&billed); usage.HasProviderUsage() {
		t.Fatalf("partial Cohere tokens became priceable: %+v", usage)
	}
}

func TestCohereRerankUsesOnlyPresentSearchUnits(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		payload   string
		wantUnits bool
		wantCount int
	}{
		{name: "present zero", payload: `{"billed_units":{"search_units":0}}`, wantUnits: true},
		{name: "missing", payload: `{"billed_units":{}}`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var meta Usage
			if err := json.Unmarshal([]byte(testCase.payload), &meta); err != nil {
				t.Fatal(err)
			}
			provider := &CohereProvider{BaseProvider: base.BaseProvider{Usage: &types.Usage{}}}
			response, apiErr := provider.ConvertToRerank(&RerankResponse{Meta: &meta}, &types.RerankRequest{Model: "rerank-v3"})
			if apiErr != nil {
				t.Fatal(apiErr)
			}
			gotUnits := response.Usage.ProviderOperationUnits != nil
			gotCount := 0
			if gotUnits {
				gotCount = *response.Usage.ProviderOperationUnits
			}
			if gotUnits != testCase.wantUnits || gotCount != testCase.wantCount {
				t.Fatalf("unexpected Cohere rerank unit evidence: %+v", response.Usage)
			}
		})
	}
}
