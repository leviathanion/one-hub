package azure

import (
	"testing"

	"one-api/common/config"
	"one-api/model"
)

func TestAzureDoesNotAuthorizeRawErrorReplay(t *testing.T) {
	proxy := ""
	provider := AzureProviderFactory{}.Create(&model.Channel{
		Type:  config.ChannelTypeAzure,
		Proxy: &proxy,
	}).(*AzureProvider)

	if provider.Requester.ReplayOpenAIErrorEnvelopes {
		t.Fatal("Azure credentials can appear in unknown error fields, so raw error replay must stay disabled")
	}
	if provider.ProviderRawJSONReplay {
		t.Fatal("error replay authorization must not enable raw success replay")
	}
}
