package relay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"one-api/common"
	"one-api/common/config"
	"one-api/model"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

func TestRejectedCandidatesStopBeforeAttemptAdmission(t *testing.T) {
	for _, test := range []struct {
		name    string
		path    string
		body    string
		channel int
	}{
		{"codex multiple choices", "/v1/chat/completions", `{"model":"gpt-5","messages":[{"role":"user","content":"hello"}],"n":2}`, config.ChannelTypeCodex},
		{"minimax speech SSE", "/v1/audio/speech", `{"model":"gpt-5","input":"hello","voice":"alloy","stream_format":"sse"}`, config.ChannelTypeMiniMax},
		{"nonchat responses compatibility", "/v1/responses", `{"model":"gpt-5","input":"hello","store":false}`, config.ChannelTypeStabilityAI},
	} {
		t.Run(test.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			originalChannels := snapshotChannelGroup()
			t.Cleanup(func() { restoreChannelGroup(originalChannels) })
			var upstreamCalls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				upstreamCalls.Add(1)
				w.WriteHeader(http.StatusInternalServerError)
			}))
			t.Cleanup(upstream.Close)
			weight, proxy := uint(1), ""
			channel := &model.Channel{
				Id: 1, Type: test.channel, Status: config.ChannelStatusEnabled,
				Key: "unused-channel-key", Group: "default", Models: "gpt-5",
				Weight: &weight, Proxy: &proxy, BaseURL: &upstream.URL, CompatibleResponse: true,
			}
			if test.channel == config.ChannelTypeCodex {
				channel.Key = `{"access_token":"access-token","account_id":"acct-123"}`
			}
			model.ChannelGroup = model.ChannelsChooser{
				Channels:   map[int]*model.ChannelChoice{1: {Channel: channel}},
				Rule:       map[string]map[string][][]int{"default": {"gpt-5": {{1}}}},
				ModelGroup: map[string]map[string]bool{"gpt-5": {"default": true}},
			}
			// RelayHandler owns token counting, Try/reserve, Claim and provider send.
			// The existing test seam proves the real HTTP entry never reaches it.
			attemptCalls := 0
			previousHandler := relayHandlerFunc
			relayHandlerFunc = func(RelayBaseInterface) (*types.OpenAIErrorWithStatusCode, bool) {
				attemptCalls++
				return common.StringErrorWrapperLocal("unexpected attempt", "unexpected_attempt", http.StatusInternalServerError), true
			}
			t.Cleanup(func() { relayHandlerFunc = previousHandler })
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.body))
			ctx.Request.Header.Set("Content-Type", "application/json")
			ctx.Set("token_group", "default")
			Relay(ctx)
			var response types.OpenAIErrorResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatalf("invalid rejection: status=%d body=%s", recorder.Code, recorder.Body)
			}
			if response.Error.Code != unsupportedCapabilityCode || attemptCalls != 0 || upstreamCalls.Load() != 0 {
				t.Fatalf("candidate rejection missed admission boundary: attempts=%d upstream=%d status=%d body=%s", attemptCalls, upstreamCalls.Load(), recorder.Code, recorder.Body)
			}
		})
	}
}
