package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"one-api/common/config"
	"one-api/common/requester"
	commonresponses "one-api/common/responses"
	"one-api/model"
	"one-api/types"
	"strings"
	"testing"
)

func TestAuxiliaryResponsesRequestsPreserveWireAndPatchOnlyModel(t *testing.T) {
	const raw = " \n{ \"model\" : \"gpt\\u002d5\", \"input\" : [ {\"role\":\"user\",\"content\":\"a\\u0020b\\n\\\"\"} ], \"future\" : {\"large\":9007199254740993123456789,\"exponent\":1e+03,\"escaped\":\"\\u0061\",\"nested\":[true,null,{\"keep\": \"  spaces  \"}]}, \"stream\":false }\t\n"
	for _, operation := range []commonresponses.Operation{commonresponses.ResponsesCompact, commonresponses.ResponsesInputTokens} {
		for _, mapped := range []bool{false, true} {
			t.Run(string(operation)+"/mapped="+map[bool]string{false: "false", true: "true"}[mapped], func(t *testing.T) {
				expected := raw
				wireModel := "gpt-5"
				if mapped {
					wireModel = "gpt-route"
					expected = strings.Replace(raw, `"gpt\u002d5"`, `"gpt-route"`, 1)
				}
				calls := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
					}
					if !mapped {
						if string(body) != expected {
							t.Errorf("unchanged wire differs\ngot: %s\nwant: %s", body, expected)
						}
					} else {
						var before, after map[string]json.RawMessage
						if err := json.Unmarshal([]byte(raw), &before); err != nil {
							t.Error(err)
						}
						if err := json.Unmarshal(body, &after); err != nil {
							t.Error(err)
						}
						if len(before) != len(after) {
							t.Errorf("field count changed")
						}
						for key, value := range before {
							if key != "model" && !bytes.Equal(value, after[key]) {
								t.Errorf("unowned field %s changed: %s -> %s", key, value, after[key])
							}
						}
						if string(after["model"]) != `"gpt-route"` {
							t.Errorf("model not patched: %s", after["model"])
						}
					}
					suffix := "/compact"
					if operation == commonresponses.ResponsesInputTokens {
						suffix = "/input_tokens"
					}
					if !strings.HasSuffix(r.URL.Path, suffix) {
						t.Errorf("path=%s", r.URL.Path)
					}
					w.Header().Set("Content-Type", "application/json")
					if operation == commonresponses.ResponsesCompact {
						io.WriteString(w, `{"id":"cmp_1","object":"response.compaction","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
					} else {
						io.WriteString(w, `{"input_tokens":1}`)
					}
				}))
				defer server.Close()
				original := requester.HTTPClient
				requester.HTTPClient = server.Client()
				defer func() { requester.HTTPClient = original }()
				proxy := ""
				provider := CreateOpenAIProvider(&model.Channel{Type: config.ChannelTypeOpenAI, Key: "sk-test", Proxy: &proxy}, server.URL)
				provider.Usage = &types.Usage{}
				request := openAIResponsesRawRequestForTest(t, raw, wireModel, false, "")
				request.Operation = operation
				if operation == commonresponses.ResponsesCompact {
					if _, err := provider.CompactResponses(context.Background(), request); err != nil {
						t.Fatal(err)
					}
				} else {
					response, err := provider.CountResponsesInputTokens(context.Background(), request)
					if err != nil {
						t.Fatal(err)
					}
					response.Body.Close()
				}
				if calls != 1 {
					t.Fatalf("upstream calls=%d", calls)
				}
			})
		}
	}
}
