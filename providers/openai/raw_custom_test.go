package openai

import (
	"testing"

	"one-api/common/config"
	"one-api/common/providerendpoint"
	"one-api/model"
)

func TestCustomRawResourceURLPreservesEndpointAndWireSuffix(t *testing.T) {
	for _, tc := range []struct{ name, base, endpoint, path, query, want string }{
		{"default", "https://compatible.example", "", "/v1/files/file%2Ftenant/content", "purpose=batch&purpose=assistants", "https://compatible.example/v1/files/file%2Ftenant/content?purpose=batch&purpose=assistants"},
		{"relative", "https://compatible.example/gateway", "/tenant%2Fa/files?fixed=a%2Fb&fixed=c", "/v1/files/file%2Ftenant/content", "after=x%2Fy&after=z", "https://compatible.example/gateway/tenant%2Fa/files/file%2Ftenant/content?fixed=a%2Fb&fixed=c&after=x%2Fy&after=z"},
		{"absolute", "https://unused.example", "https://managed.example/objects/?tenant=one", "/v1/files/file%2Ftenant", "limit=5", "https://managed.example/objects/file%2Ftenant?tenant=one&limit=5"},
		{"cloudflare", "https://gateway.ai.cloudflare.com/v1/account/gateway/openai", "", "/v1/files", "", "https://gateway.ai.cloudflare.com/v1/account/gateway/openai/files"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proxy := ""
			channel := &model.Channel{Type: config.ChannelTypeCustom, Proxy: &proxy, Plugin: model.NewCustomEndpointPlugin()}
			channel.Plugin.Data()["endpoints"][providerendpoint.Files] = (providerendpoint.Setting{Enabled: true, UpstreamURL: tc.endpoint}).Data()
			provider := CreateOpenAIProvider(channel, tc.base)
			got, err := provider.BuildRawRelayURL(tc.path, tc.query)
			if err != nil || got != tc.want {
				t.Fatalf("URL=%q err=%v, want %q", got, err, tc.want)
			}
		})
	}
}

func TestCustomRawResourceFamilyIsExplicitlyEnabled(t *testing.T) {
	proxy := ""
	channel := &model.Channel{Type: config.ChannelTypeCustom, Proxy: &proxy, Plugin: model.NewCustomEndpointPlugin()}
	provider := CreateOpenAIProvider(channel, "https://compatible.example")
	for _, id := range []string{providerendpoint.Files, providerendpoint.Uploads, providerendpoint.Conversations, providerendpoint.Batches, providerendpoint.FineTuning, providerendpoint.Assistants, providerendpoint.Threads, providerendpoint.VectorStores} {
		definition, _ := providerendpoint.Find(id)
		if _, err := provider.BuildRawRelayURL(definition.DefaultPath, ""); err == nil {
			t.Fatalf("new resource family %s enabled by default", id)
		}
		delete(channel.Plugin.Data()["endpoints"], id)
		if _, err := provider.BuildRawRelayURL(definition.DefaultPath, ""); err == nil {
			t.Fatalf("missing resource family %s enabled", id)
		}
		channel.Plugin.Data()["endpoints"][id] = (providerendpoint.Setting{Enabled: true}).Data()
		if _, err := provider.BuildRawRelayURL(definition.DefaultPath+"/id", ""); err != nil {
			t.Fatalf("enabled resource family %s rejected: %v", id, err)
		}
	}
	for _, path := range []string{"/v1/files_extra", "/v1/models", "//evil.example/v1/files", "https://evil.example/v1/files", "/v1/files/bad%path"} {
		if _, err := provider.BuildRawRelayURL(path, ""); err == nil {
			t.Fatalf("unregistered or invalid path accepted: %q", path)
		}
	}
}
