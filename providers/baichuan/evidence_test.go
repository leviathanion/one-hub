package baichuan

import (
	"testing"

	"one-api/model"
	"one-api/providers/base"
	"one-api/providers/openai"
)

func TestBaichuanSearchModelsAreRejectedBeforeProviderWork(t *testing.T) {
	provider := &BaichuanProvider{OpenAIProvider: openai.OpenAIProvider{BaseProvider: base.BaseProvider{Channel: &model.Channel{Other: `{}`}}}}
	if apiErr := provider.rejectUnpricedSearch("Baichuan-M3-Plus"); apiErr == nil || apiErr.Code != "baichuan_search_billing_unsupported" {
		t.Fatalf("automatic-search model was not rejected: %+v", apiErr)
	}
	if apiErr := provider.rejectUnpricedSearch("Baichuan4"); apiErr != nil {
		t.Fatalf("ordinary Baichuan model was rejected: %+v", apiErr)
	}
	custom := `{"with_search_enhance":true}`
	if _, err := (BaichuanProviderFactory{}).AssessChatRequest(&model.Channel{CustomParameter: &custom}, "Baichuan4", nil); err == nil {
		t.Fatal("factory missed custom search enhancement before provider construction")
	}
}
