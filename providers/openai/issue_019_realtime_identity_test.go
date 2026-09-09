package openai

import (
	"testing"

	"one-api/common/config"
	"one-api/model"
)

func TestFixI019_RealtimeNamedURLConstructionKeepsExistingWire(t *testing.T) {
	const modelName = "gpt-realtime-test"
	for _, channelType := range []int{config.ChannelTypeCustom, config.ChannelTypeOpenAI} {
		for _, tc := range []struct{ name, base, uri, want string }{
			{"relative_query", "https://base.example/root/", "/tenant/responses?tenant=one", "wss://base.example/root/tenant/responses?tenant=one?model=gpt-realtime-test"},
			{"absolute", "https://base.example/root/", "https://owner.example/tenant/responses", "wss://base.example/roothttps://owner.example/tenant/responses?model=gpt-realtime-test"},
			{"leading_space", "https://base.example/root/", " /tenant/responses", "wss://base.example/root /tenant/responses?model=gpt-realtime-test"},
			{"trailing_space", "http://base.example/root/", "/tenant/responses ", "ws://base.example/root/tenant/responses ?model=gpt-realtime-test"},
			{"one_trailing_slash_removed", "https://base.example/root//", "/tenant/responses", "wss://base.example/root//tenant/responses?model=gpt-realtime-test"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				proxy := ""
				channel := &model.Channel{Type: channelType, BaseURL: &tc.base, Proxy: &proxy}
				provider := CreateOpenAIProvider(channel, channel.GetBaseURL())
				if got := provider.GetFullRequestURL(tc.uri, modelName); got != tc.want {
					t.Fatalf("-realtime 命名分支 URL=%q，want %q", got, tc.want)
				}
			})
		}
	}
}
