package azure_v1

import (
	"testing"

	"one-api/common/config"
	"one-api/model"
)

func TestAzureV1DoesNotAuthorizeRawErrorReplay(t *testing.T) {
	proxy := ""
	provider := AzureV1ProviderFactory{}.Create(&model.Channel{
		Type:  config.ChannelTypeAzureV1,
		Proxy: &proxy,
	}).(*AzureV1Provider)

	if provider.Requester.ReplayOpenAIErrorEnvelopes {
		t.Fatal("Azure V1 credentials can appear in unknown error fields, so raw error replay must stay disabled")
	}
	if provider.ProviderRawJSONReplay {
		t.Fatal("error replay authorization must not enable raw success replay")
	}
}
