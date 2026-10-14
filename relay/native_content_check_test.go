package relay

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"one-api/common"
	"one-api/common/config"
	"one-api/common/requester"
	"one-api/model"
	"one-api/providers"
	"one-api/safty"
	"one-api/safty/providers/keyword"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

func TestNativeContentCheckDecodesTextAndPreservesWire(t *testing.T) {
	previousOptions, previousTools := config.GlobalOption, safty.Tools
	t.Cleanup(func() { config.GlobalOption, safty.Tools = previousOptions, previousTools })
	manager := config.NewOptionManager()
	enabled, toolName, keywords := true, "Keyword", "blocked"
	manager.RegisterBool("EnableSafe", &enabled)
	manager.RegisterString("SafeToolName", &toolName)
	manager.RegisterString("SafeKeyWords", &keywords)
	config.GlobalOption = manager
	safty.Tools = map[string]safty.SaftyTool{"Keyword": keyword.NewKeywordChecker()}

	for _, protocol := range []string{"claude", "gemini"} {
		for _, stream := range []bool{false, true} {
			for _, test := range []struct {
				name, text string
				future     bool
				disabled   bool
				blocked    bool
			}{
				{name: "plain keyword", text: `blocked`, blocked: true},
				{name: "escaped keyword", text: `\u0062locked`, blocked: true},
				{name: "future union keyword", text: `\u0062locked`, future: true, blocked: true},
				{name: "escaped allowed text", text: `\u0068ello`},
				{name: "future union allowed", text: `\u0068ello`, future: true},
				{name: "disabled check", text: `\u0062locked`, disabled: true},
			} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", protocol, stream, test.name), func(t *testing.T) {
					if _, err := manager.PublishRuntimeOverrides(manager.RuntimeSnapshot().Version()+1, map[string]string{"EnableSafe": fmt.Sprint(!test.disabled)}); err != nil {
						t.Fatal(err)
					}
					var calls atomic.Int32
					var forwarded string
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						calls.Add(1)
						body, _ := io.ReadAll(r.Body)
						forwarded = string(body)
						if stream {
							w.Header().Set("Content-Type", "text/event-stream")
							if protocol == "claude" {
								_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
							} else {
								_, _ = io.WriteString(w, "data: {\"candidates\":[{\"index\":0,\"finishReason\":\"STOP\"}]}\n\n")
							}
							return
						}
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, `{ "usage":{"input_tokens":1,"output_tokens":1}, "future_response":1e+3 }`)
					}))
					t.Cleanup(upstream.Close)
					previousClient := requester.HTTPClient
					requester.HTTPClient = upstream.Client()
					t.Cleanup(func() { requester.HTTPClient = previousClient })

					path, contentField, channelType := "/claude/v1/messages", "messages", config.ChannelTypeAnthropic
					content := fmt.Sprintf(`[{"role":"user","content":[{"type":"text","text":"hello"},{"type":"text","text":"%s"}]}]`, test.text)
					if protocol == "gemini" {
						action := "generateContent"
						if stream {
							action = "streamGenerateContent"
						}
						path, contentField, channelType = "/gemini/v1beta/models/native-test:"+action, "contents", config.ChannelTypeGemini
						content = fmt.Sprintf(`[{"role":"user","parts":[{"text":"hello"},{"text":"%s"}]}]`, test.text)
					}
					if test.future {
						content = fmt.Sprintf(`{"future_union":{"text":"%s","number":1e+10000}}`, test.text)
					}
					raw := fmt.Sprintf(`{ "%s":%s, "future":{"integer":9007199254740993,"exponent":1e+3} }`, contentField, content)
					if protocol == "claude" {
						raw = fmt.Sprintf(`{ "model":"native-test", "max_tokens":32, "stream":%t, "%s":%s, "future":{"integer":9007199254740993,"exponent":1e+3} }`, stream, contentField, content)
					}
					recorder := httptest.NewRecorder()
					ctx, _ := gin.CreateTestContext(recorder)
					ctx.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(raw))
					ctx.Request.Header.Set("Content-Type", "application/json")
					ctx.Params = gin.Params{{Key: "model", Value: strings.TrimPrefix(path, "/gemini/v1beta/models/")}}
					proxy, baseURL := "", upstream.URL
					channel := &model.Channel{Type: channelType, Key: "test-key", Proxy: &proxy, BaseURL: &baseURL}
					provider := providers.GetProvider(channel, ctx)
					provider.SetUsage(&types.Usage{})
					var relay RelayBaseInterface
					if protocol == "claude" {
						r := NewRelayClaudeOnly(ctx)
						r.provider, r.modelName = provider, "native-test"
						relay = r
					} else {
						r := NewRelayGeminiOnly(ctx)
						r.provider, r.modelName = provider, "native-test"
						relay = r
					}
					if err := relay.setRequest(); err != nil {
						t.Fatal(err)
					}
					if err := finalizeSelectedProviderRequest(relay); err != nil {
						t.Fatal(err)
					}
					apiErr, done := relay.send()
					if test.blocked {
						if apiErr == nil || apiErr.StatusCode != http.StatusBadRequest || !apiErr.LocalError || !done || calls.Load() != 0 {
							t.Fatalf("违规内容未在上游调用前拦截：err=%+v done=%t calls=%d", apiErr, done, calls.Load())
						}
					} else if apiErr != nil || calls.Load() != 1 || forwarded != raw {
						t.Fatalf("内容观察改变了原生转发：err=%+v calls=%d\nwant=%s\ngot=%s", apiErr, calls.Load(), raw, forwarded)
					}
					if canonical, ok := common.GetCanonicalRequestBody(ctx); !ok || string(canonical) != raw {
						t.Fatalf("内容检查修改了请求字节：%s", canonical)
					}
				})
			}
		}
	}
}
