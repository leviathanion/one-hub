package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"one-api/common/config"
	"one-api/common/requester"
	"one-api/model"
	"one-api/types"
)

func TestAstraResponsesPreserveAsyncConfigurationAndCacheWire(t *testing.T) {
	requestBody := `{"model":"gpt-6-astra","store":false,"reasoning":{"effort":"low"},"prompt_cache_options":{"mode":"explicit","ttl":"30m"},"tools":[{"type":"function","name":"lookup","async":true,"parameters":{"type":"object"}},{"type":"custom","name":"shell","async":true,"format":{"type":"text"}}],"input":[{"type":"configuration_update","reasoning":{"effort":"high"}},{"role":"user","content":[{"type":"input_text","text":"continue","prompt_cache_breakpoint":{"mode":"explicit"},"future":9007199254740993}]},{"type":"custom_tool_call_output","call_id":"call_old","output":"saved result"}]}`
	responseBody := `{"id":"resp_astra","model":"gpt-6-astra","status":"completed","output":[{"type":"function_call","call_id":"call_fn","name":"lookup","arguments":"{}","async":true},{"type":"custom_tool_call","call_id":"call_custom","name":"shell","input":"pwd","async":true}],"usage":{"input_tokens":20,"output_tokens":3,"total_tokens":23,"input_tokens_details":{"cached_tokens":4,"cache_write_tokens":5}}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		if !bytes.Contains(body, []byte(`9007199254740993`)) {
			t.Error("future input number lost precision")
		}
		var want, got map[string]json.RawMessage
		if json.Unmarshal([]byte(requestBody), &want) != nil || json.Unmarshal(body, &got) != nil {
			t.Error("invalid JSON")
		}
		for _, key := range []string{"tools", "input", "reasoning", "prompt_cache_options"} {
			var a, b any
			_ = json.Unmarshal(want[key], &a)
			_ = json.Unmarshal(got[key], &b)
			if !reflect.DeepEqual(a, b) {
				t.Errorf("%s changed: %s", key, got[key])
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, responseBody)
	}))
	t.Cleanup(server.Close)
	old := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = old })
	proxy := ""
	p := CreateOpenAIProvider(&model.Channel{Type: config.ChannelTypeOpenAI, Key: "sk-test", Proxy: &proxy}, server.URL)
	p.SetProviderRawJSONReplay(true)
	p.Usage = &types.Usage{}
	result, apiErr := p.CreateResponses(context.Background(), openAIResponsesRawRequestForTest(t, requestBody, "gpt-6-astra", false, ""))
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	if string(result.ReplayProviderRawJSON()) != responseBody {
		t.Fatal("async output wire changed")
	}
	if p.Usage.PromptTokensDetails.CachedTokens != 4 || p.Usage.PromptTokensDetails.CacheWriteTokens != 5 {
		t.Fatalf("cache usage lost: %+v", p.Usage)
	}
}
