package relay_util

import (
	"encoding/json"
	"testing"

	"one-api/common/config"
	"one-api/providers/gemini"
)

func TestGeminiToolImageSettlementRequiresDifferentlyPricedPartition(t *testing.T) {
	for _, test := range []struct {
		name       string
		image      bool
		imageRatio float64
		details    string
		confirm    bool
		quota      int64
	}{
		{name: "image partition missing", image: true, imageRatio: 2},
		{name: "image partition reported", image: true, imageRatio: 2, details: `,"promptTokensDetails":[{"modality":"TEXT","tokenCount":3},{"modality":"IMAGE","tokenCount":2}]`, confirm: true, quota: 7},
		{name: "same image price needs only totals", image: true, imageRatio: 1, confirm: true, quota: 5},
		{name: "text tool result needs no image partition", imageRatio: 2, confirm: true, quota: 5},
	} {
		t.Run(test.name, func(t *testing.T) {
			parts := ""
			if test.image {
				parts = `,"parts":[{"inlineData":{"mimeType":"image/png","data":"aW1hZ2U="}}]`
			}
			request := &gemini.NativeRequest{Raw: []byte(`{"contents":[{"role":"user","parts":[{"functionResponse":{"name":"inspect","response":{"result":"done"}` + parts + `}}]}]}`)}
			var reported gemini.GeminiUsageMetadata
			if err := json.Unmarshal([]byte(`{"promptTokenCount":5,"candidatesTokenCount":0,"totalTokenCount":5`+test.details+`}`), &reported); err != nil {
				t.Fatal(err)
			}
			usage := gemini.ConvertOpenAIUsage(&reported, "gemini-model")
			request.ApplyUsageRequirements(&usage)
			quota := usagePartitionQuota(t, usagePartitionPrice(t, "gemini-model", 1, 1, map[string]float64{
				config.UsageExtraInputImageTokens: test.imageRatio,
			}), "gemini-model")
			decision := quota.EvaluateProviderUsage(&usage)
			if decision.Confirm != test.confirm || decision.FinalQuota != test.quota {
				t.Fatalf("工具图片结算错误: decision=%+v usage=%+v", decision, usage)
			}
			wantStatus := PriceComponentMissingEvidence
			if test.confirm {
				wantStatus = PriceComponentPriceable
			}
			if got := componentStatuses(decision.Components)["tokens"]; got != wantStatus {
				t.Fatalf("token 证据状态 = %s, 期望 %s: %+v", got, wantStatus, decision)
			}
		})
	}
}
