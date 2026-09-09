package claude

import (
	"encoding/json"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"net/http/httptest"
	"one-api/types"
	"testing"
)

func TestClaudeActualSpeedIsNotRequestedSpeed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		assert.Contains(t, string(body), `"speed":"fast"`)
		assert.Contains(t, string(body), `"future_option"`)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_speed","type":"message","model":"claude-test","content":[],"usage":{"input_tokens":100,"output_tokens":10,"speed":"standard","service_tier":"priority"},"future_response":true}`)
	}))
	defer server.Close()
	provider, request := newI025NativeClaudeProviderForTest(t, server, `{"model":"claude-test","max_tokens":64,"messages":[],"speed":"fast","future_option":true}`)
	_, apiErr := provider.CreateClaudeChat(request)
	require.Nil(t, apiErr)
	assert.Equal(t, "standard", provider.GetUsage().Speed)
	assert.Equal(t, "priority", provider.GetUsage().ServiceTier)
}

func TestClaudeStreamSpeedSurvivesOmissionAndFlagsConflict(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(map[bool]string{false: "omitted", true: "conflict"}[conflict], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-test\",\"usage\":{\"input_tokens\":100,\"output_tokens\":0,\"speed\":\"fast\"}}}\n\n")
				delta := `{"output_tokens":10}`
				if conflict {
					delta = `{"output_tokens":10,"speed":"standard"}`
				}
				_, _ = io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":"+delta+"}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			}))
			defer server.Close()
			provider, request := newI025NativeClaudeProviderForTest(t, server, `{"model":"claude-test","max_tokens":64,"messages":[],"stream":true}`)
			stream, apiErr := provider.CreateClaudeChatStream(request)
			require.Nil(t, apiErr)
			drainI025ClaudeStream(t, stream)
			usage := provider.GetUsage()
			assert.Equal(t, "fast", usage.Speed)
			assert.Equal(t, conflict, usage.SpeedConflict)
			assert.True(t, usage.HasProviderBaseUsage())
		})
	}
}

func TestClaudeSpeedSharedConversion(t *testing.T) {
	var raw Usage
	require.NoError(t, json.Unmarshal([]byte(`{"input_tokens":1,"output_tokens":2,"speed":"fast"}`), &raw))
	usage := &types.Usage{}
	require.True(t, ClaudeUsageToOpenaiUsage(&raw, usage))
	assert.Equal(t, "fast", usage.Speed)
	event := &types.UsageEvent{Speed: usage.Speed, SpeedConflict: usage.SpeedConflict, ProviderTokenEvidence: true, InputTokens: 1, OutputTokens: 2}
	copy := event.Clone()
	copy.Merge(&types.UsageEvent{Speed: "standard"})
	assert.True(t, copy.ToChatUsage().SpeedConflict)
	assert.False(t, event.SpeedConflict)
}
