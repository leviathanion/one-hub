package providers

import (
	"encoding/base64"
	"errors"
	"testing"

	"one-api/common/config"
	"one-api/model"
	"one-api/providers/base"
	"one-api/providers/claude"
	"one-api/types"
)

type providerRemoteMediaFetcher struct {
	calls int
	err   error
}

type sideEffectingAssessmentFactory struct {
	createCalls int
}

func (f *sideEffectingAssessmentFactory) Create(*model.Channel) base.ProviderInterface {
	f.createCalls++
	return nil
}

func (*sideEffectingAssessmentFactory) AssessChatRemoteMedia(*model.Channel, *types.ChatCompletionRequest, base.ChatRemoteMediaSummary) (base.RemoteMediaMode, error) {
	return base.RemoteMediaPassURL, nil
}

func (*sideEffectingAssessmentFactory) AssessNativeClaudeRemoteMedia(*model.Channel, *claude.ClaudeRequest, claude.NativeRemoteMediaSummary) (base.RemoteMediaMode, error) {
	return base.RemoteMediaPassURL, nil
}

func (f *providerRemoteMediaFetcher) Fetch(string) (string, []byte, error) {
	f.calls++
	if f.err != nil {
		return "", nil, f.err
	}
	return "image/png", []byte("provider-image"), nil
}

func providerMediaRequest(rawURL string) *types.ChatCompletionRequest {
	return &types.ChatCompletionRequest{
		Model: "gemini-2.5-pro",
		Messages: []types.ChatCompletionMessage{{
			Role: types.ChatMessageRoleUser,
			Content: []types.ChatMessagePart{{
				Type:     types.ContentTypeImageURL,
				ImageURL: &types.ChatMessageImageURL{URL: rawURL},
			}},
		}},
	}
}

func remoteMediaTestChannel(channelType int) *model.Channel {
	proxy := ""
	return &model.Channel{Type: channelType, Proxy: &proxy}
}

func TestAssessChatRemoteMediaUsesProviderLocalPolicyWithoutFetching(t *testing.T) {
	request := providerMediaRequest("https://example.com/a.png")
	for name, test := range map[string]struct {
		channel *model.Channel
		want    base.RemoteMediaMode
	}{
		"OpenAI pass":        {channel: remoteMediaTestChannel(config.ChannelTypeOpenAI), want: base.RemoteMediaPassURL},
		"Azure pass":         {channel: remoteMediaTestChannel(config.ChannelTypeAzure), want: base.RemoteMediaPassURL},
		"Azure v1 pass":      {channel: remoteMediaTestChannel(config.ChannelTypeAzureV1), want: base.RemoteMediaPassURL},
		"Gemini materialize": {channel: remoteMediaTestChannel(config.ChannelTypeGemini), want: base.RemoteMediaMaterialize},
		"unknown OpenAI-compatible fallback": {
			channel: func() *model.Channel {
				channel := remoteMediaTestChannel(987654)
				baseURL := "https://compatible.example/v1"
				channel.BaseURL = &baseURL
				return channel
			}(),
			want: base.RemoteMediaPassURL,
		},
	} {
		t.Run(name, func(t *testing.T) {
			mode, summary, err := AssessChatRemoteMedia(test.channel, request)
			if err != nil || mode != test.want || summary.RemoteURLs != 1 {
				t.Fatalf("mode=%v summary=%+v err=%v", mode, summary, err)
			}
		})
	}
}

func TestAssessChatRemoteMediaDoesNotConstructProvider(t *testing.T) {
	const channelType = 987653
	factory := &sideEffectingAssessmentFactory{}
	previous, existed := providerFactories[channelType]
	providerFactories[channelType] = factory
	t.Cleanup(func() {
		if existed {
			providerFactories[channelType] = previous
		} else {
			delete(providerFactories, channelType)
		}
	})

	mode, _, err := AssessChatRemoteMedia(remoteMediaTestChannel(channelType), providerMediaRequest("https://example.com/a.png"))
	if err != nil || mode != base.RemoteMediaPassURL {
		t.Fatalf("candidate media assessment failed: mode=%v err=%v", mode, err)
	}
	if factory.createCalls != 0 {
		t.Fatalf("candidate assessment constructed provider %d times", factory.createCalls)
	}
}

func TestAssessNativeClaudeRemoteMediaDoesNotConstructProvider(t *testing.T) {
	const channelType = 987652
	factory := &sideEffectingAssessmentFactory{}
	previous, existed := providerFactories[channelType]
	providerFactories[channelType] = factory
	t.Cleanup(func() {
		if existed {
			providerFactories[channelType] = previous
		} else {
			delete(providerFactories, channelType)
		}
	})
	request := &claude.ClaudeRequest{Messages: []claude.Message{{Content: []any{
		map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": "https://example.com/a.png"}},
	}}}}
	mode, _, err := AssessNativeClaudeRemoteMedia(remoteMediaTestChannel(channelType), request)
	if err != nil || mode != base.RemoteMediaPassURL {
		t.Fatalf("candidate native Claude assessment failed: mode=%v err=%v", mode, err)
	}
	if factory.createCalls != 0 {
		t.Fatalf("candidate native Claude assessment constructed provider %d times", factory.createCalls)
	}
}

func TestAssessChatRemoteMediaDefaultsUndeclaredProviderToReject(t *testing.T) {
	_, _, err := AssessChatRemoteMedia(remoteMediaTestChannel(config.ChannelTypeTencent), providerMediaRequest("https://example.com/a.png"))
	var capabilityErr *base.RequestCapabilityError
	if !errors.As(err, &capabilityErr) || capabilityErr.Param != "messages" {
		t.Fatalf("expected provider-local default reject, got %v", err)
	}
}

func TestPrepareChatRemoteMediaPassURLNeverFetches(t *testing.T) {
	request := providerMediaRequest("https://example.com/a.png")
	provider := createProvider(remoteMediaTestChannel(config.ChannelTypeOpenAI))
	fetcher := &providerRemoteMediaFetcher{err: errors.New("must not fetch")}
	if err := PrepareChatRemoteMedia(provider, request, fetcher); err != nil {
		t.Fatal(err)
	}
	if fetcher.calls != 0 || request.Messages[0].ParseContent()[0].ImageURL.URL != "https://example.com/a.png" {
		t.Fatalf("PassURL fetched or changed input: calls=%d request=%+v", fetcher.calls, request)
	}
}

func TestPrepareChatRemoteMediaMaterializesSelectedProviderOnce(t *testing.T) {
	request := providerMediaRequest("https://example.com/a.png")
	provider := createProvider(remoteMediaTestChannel(config.ChannelTypeGemini))
	fetcher := &providerRemoteMediaFetcher{}
	if err := PrepareChatRemoteMedia(provider, request, fetcher); err != nil {
		t.Fatal(err)
	}
	if fetcher.calls != 1 {
		t.Fatalf("materialize fetch calls=%d, want 1", fetcher.calls)
	}
}

func TestPrepareChatRemoteMediaDataURINeverFetchesAndRejectsInvalid(t *testing.T) {
	provider := createProvider(remoteMediaTestChannel(config.ChannelTypeGemini))
	valid := providerMediaRequest("data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("local")))
	fetcher := &providerRemoteMediaFetcher{err: errors.New("must not fetch")}
	if err := PrepareChatRemoteMedia(provider, valid, fetcher); err != nil {
		t.Fatal(err)
	}
	if fetcher.calls != 0 {
		t.Fatalf("data URI triggered %d network calls", fetcher.calls)
	}
	invalid := providerMediaRequest("data:image/png;base64,not-valid!")
	if err := PrepareChatRemoteMedia(provider, invalid, fetcher); err == nil {
		t.Fatal("expected invalid data URI to fail")
	}
	if fetcher.calls != 0 {
		t.Fatalf("invalid data URI triggered %d network calls", fetcher.calls)
	}
}
