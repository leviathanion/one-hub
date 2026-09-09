package minimax

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"one-api/common/requester"
	"one-api/model"
	"one-api/types"
)

type minimaxProfileRoundTripper func(*http.Request) (*http.Response, error)

func (f minimaxProfileRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestMiniMaxBufferedSpeechKeepsWorkActionTimeout(t *testing.T) {
	originalClient := requester.HTTPClient
	requester.HTTPClient = &http.Client{
		Timeout: 10 * time.Millisecond,
		Transport: minimaxProfileRoundTripper(func(req *http.Request) (*http.Response, error) {
			select {
			case <-time.After(40 * time.Millisecond):
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"base_resp":{"status_code":0},"data":{"audio":"00"},"extra_info":{"audio_format":"mp3","audio_size":1}}`)),
					Request:    req,
				}, nil
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
		}),
	}
	t.Cleanup(func() { requester.HTTPClient = originalClient })

	baseURL, proxy := "https://minimax.test", ""
	provider := MiniMaxProviderFactory{}.Create(&model.Channel{BaseURL: &baseURL, Proxy: &proxy}).(*MiniMaxProvider)
	provider.SetUsage(&types.Usage{})
	response, apiErr := provider.CreateSpeech(&types.SpeechAudioRequest{
		Model: "speech-01",
		Input: "hello",
		Voice: json.RawMessage(`"alloy"`),
	})
	if apiErr == nil || response != nil {
		t.Fatalf("buffered MiniMax speech escaped WorkAction timeout: response=%v err=%+v", response, apiErr)
	}
}
