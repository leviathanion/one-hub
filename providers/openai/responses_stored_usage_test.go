package openai

import (
	"encoding/json"
	"testing"

	commonresponses "one-api/common/responses"
	"one-api/types"
)

func TestStoredResponsesUsageCountsToolsOnce(t *testing.T) {
	for _, tokenUsage := range []string{"", `,"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}`} {
		t.Run(tokenUsage, func(t *testing.T) {
			var response types.OpenAIResponsesResponses
			wire := `{"id":"resp_tools","status":"completed","model":"gpt-5","tools":[{"type":"web_search","search_context_size":"high"},{"type":"image_generation","model":"gpt-image-2"}],"output":[{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search"}},{"type":"image_generation_call","id":"img_1","status":"completed","quality":"high","size":"1024x1024"}]` + tokenUsage + `}`
			if err := json.Unmarshal([]byte(wire), &response); err != nil {
				t.Fatal(err)
			}
			baseline, observed := &types.Usage{}, &types.Usage{}
			commonresponses.ApplyResponsesUsage(baseline, &response)
			for i := 0; i < 3; i++ {
				ObserveStoredResponsesUsage(&response, observed)
				for _, key := range []string{
					types.BuildExtraBillingKey(types.APIToolTypeWebSearch, "high"),
					types.BuildExtraBillingKey(types.APIToolTypeImageGeneration, "gpt-image-2|high|1024x1024|0"),
				} {
					if baseline.ExtraBilling[key].CallCount != 1 || observed.ExtraBilling[key].CallCount != 1 {
						t.Fatalf("observation %d: key=%s baseline=%+v observed=%+v", i, key, baseline.ExtraBilling, observed.ExtraBilling)
					}
				}
				if observed.TotalTokens != baseline.TotalTokens {
					t.Fatalf("tokens changed: %d != %d", observed.TotalTokens, baseline.TotalTokens)
				}
			}
		})
	}
}
