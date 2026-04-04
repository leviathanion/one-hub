package providers_test

import (
	"testing"

	"one-api/common/config"
	"one-api/common/test"
	_ "one-api/common/test/init"
	"one-api/providers"
	klingprovider "one-api/providers/kling"
)

func TestGetProviderReturnsKlingProvider(t *testing.T) {
	channel := test.GetChannel(config.ChannelTypeKling, "https://api.klingai.com", "", "", "")

	provider := providers.GetProvider(&channel, nil)
	if provider == nil {
		t.Fatal("expected kling provider factory to return a provider")
	}
	if _, ok := provider.(*klingprovider.KlingProvider); !ok {
		t.Fatalf("expected kling provider, got %T", provider)
	}
}
