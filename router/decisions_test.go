package router

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"one-api/common/config"
	ratelimit "one-api/common/limit"
	"one-api/model"

	"gorm.io/datatypes"
)

func TestDecisionsRoutePreservesNativeJSONAndSettlesOnce(t *testing.T) {
	const alias = "decisions-luna"
	const body = `{
  "model": "decisions-luna",
  "input": [{"role":"user","content":[{"type":"input_json","data":{"order":9007199254740993,"file_id":"business-data"}}]}],
  "questions": [
    {"type":"predicate","name":"late","instructions":"Is there a delivery delay?"},
    {"type":"choice","name":"route","instructions":[{"type":"input_image","image_url":"https://example.com/label.png"}],"choices":[{"value":true},{"value":"true"}]},
    {"type":"score","name":"severity","instructions":"How severe is the issue?","levels":[{"value":0,"label":"none"},{"value":1,"label":"high"}]}
  ],
  "prompt_cache_options":{"mode":"implicit"},
  "future_option":{"value":null}
}`
	const result = `{"model":"gpt-6-luna","answers":[{"type":"predicate","name":"late","probability":0.2},{"type":"choice","name":"route","choice":true},{"type":"refusal","name":"severity"},{"type":"future_answer","value":9007199254740993}],"usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":40,"cache_write_tokens":10},"output_tokens":6,"output_tokens_details":{"reasoning_tokens":2},"total_tokens":106}}`
	const withoutUsage = `{"model":"gpt-6-luna","answers":[],"usage":{"input_tokens":"unavailable"},"extension":true}`
	const rejected = `{"error":{"type":"invalid_request_error","message":"Invalid question","code":"invalid_question","param":"questions","access_token":"upstream-secret"},"future_error":{"detail":true}}`

	var original map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &original); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	fixture := newLongLivedRouteFixtureWithHTTP(t, false, true, func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/v1/decisions" || r.Header.Get("Authorization") != "Bearer upstream-secret" {
			t.Errorf("上游接口或渠道不符：method=%s path=%s", r.Method, r.URL.Path)
		}
		var sent map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
			t.Error(err)
		}
		if string(sent["model"]) != `"gpt-6-luna"` || len(sent) != len(original) {
			t.Errorf("上游模型映射或字段数量不符：%s，%d 个字段", sent["model"], len(sent))
		}
		for name, value := range original {
			if name != "model" && !bytes.Equal(value, sent[name]) {
				t.Errorf("上游字段 %s 被改写", name)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "req_decision")
		switch call {
		case 1:
			_, _ = io.WriteString(w, result)
		case 2:
			_, _ = io.WriteString(w, withoutUsage)
		case 3:
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = io.WriteString(w, rejected)
		case 4:
			w.Header().Set("Content-Length", strconv.Itoa(len(result)+20))
			_, _ = io.WriteString(w, result)
		default:
			t.Errorf("Decisions 请求被额外提交：%d", call)
			w.WriteHeader(http.StatusBadGateway)
		}
	})
	configureDecisionsRouteTest(t, fixture, alias, model.TokensPriceType, 0)

	status, _, _ := postDecision(t, fixture, "", body)
	if status != http.StatusUnauthorized || calls.Load() != 0 {
		t.Fatalf("无令牌的请求访问了 Decisions：状态 %d，调用 %d 次", status, calls.Load())
	}
	status, _, _ = postDecision(t, fixture, fixture.token, `{"model":"decisions-luna","model":"other","input":"test","questions":[]}`)
	if status != http.StatusBadRequest || calls.Load() != 0 {
		t.Fatalf("有歧义的模型进入了选路：状态 %d，调用 %d 次", status, calls.Load())
	}

	status, headers, delivered := postDecision(t, fixture, fixture.token, body)
	if status != http.StatusOK || headers.Get("X-Request-Id") != "req_decision" || delivered != result || calls.Load() != 1 {
		t.Fatalf("原生决策响应未完整交付：状态 %d，调用 %d 次，正文 %s", status, calls.Load(), delivered)
	}
	status, _, delivered = postDecision(t, fixture, fixture.token, body)
	if status != http.StatusOK || delivered != withoutUsage || calls.Load() != 2 {
		t.Fatalf("无法读取用量的决策响应改变：状态 %d，正文 %s", status, delivered)
	}
	status, _, delivered = postDecision(t, fixture, fixture.token, body)
	if status != http.StatusUnprocessableEntity || !strings.Contains(delivered, `"code":"invalid_question"`) ||
		!strings.Contains(delivered, `"future_error":{"detail":true}`) || strings.Contains(delivered, "upstream-secret") || calls.Load() != 3 {
		t.Fatalf("上游错误未安全交付：状态 %d，调用 %d 次，正文 %s", status, calls.Load(), delivered)
	}
	status, _, delivered = postDecision(t, fixture, fixture.token, body)
	if status < http.StatusInternalServerError || calls.Load() != 4 {
		t.Fatalf("不完整的回复被认可或重试：状态 %d，调用 %d 次，正文 %s", status, calls.Load(), delivered)
	}

	// 100 个输入已含 40 个缓存读、10 个缓存写。倍率调整后按 75 个输入结算。
	fixture.awaitQuota(t, 150)
	var logs []model.Log
	if err := fixture.db.Where("type = ? AND user_id = ?", model.LogTypeConsume, fixture.userID).Find(&logs).Error; err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 || logs[0].Quota != 150 || logs[0].ModelName != alias || logs[0].PromptTokens != 100 || logs[0].CompletionTokens != 6 {
		t.Fatalf("Decisions 消费记录不符：%+v", logs)
	}
}

func TestDecisionsChargesOnlySupportedUsageEvidence(t *testing.T) {
	const (
		alias         = "decisions-luna"
		cache         = `"cached_tokens":1024,"cache_write_tokens":128`
		withReasoning = `"output_tokens":6,"output_tokens_details":{"reasoning_tokens":2},"total_tokens":2054`
	)
	body := `{"model":"decisions-luna","input":"` + strings.Repeat(" blue", 2048) + `","questions":[{"type":"predicate","name":"color","instructions":"是否提到颜色？"}]}`
	// 普通输入 896、缓存读 1024、写 128，共计 2688 额度。4 个普通输出和 2 个推理输出另计 48。
	for _, tc := range []struct {
		name, cache, output, priceType string
		outputPrice                    float64
		charge                         int
	}{
		{"缓存写超过输入总量", `"cached_tokens":0,"cache_write_tokens":2049`, withReasoning, model.TokensPriceType, 4, 0},
		{"扩展键不覆盖缓存读", cache + `,"CACHED_TOKENS":0`, withReasoning, model.TokensPriceType, 4, 2736},
		{"推理有差价但缺少用量", cache, `"output_tokens":6,"total_tokens":2054`, model.TokensPriceType, 4, 0},
		{"免费输出无需推理明细", cache, `"output_tokens":6,"total_tokens":2054`, model.TokensPriceType, 0, 2688},
		{"零输出已确定推理费用", cache, `"output_tokens":0,"total_tokens":2048`, model.TokensPriceType, 4, 2688},
		{"按次价缺少缓存写明细仍收一次", `"cached_tokens":1024`, withReasoning, model.TimesPriceType, 0, 2000},
		{"按次价缺少基础输出用量时退款", cache, `"total_tokens":2054`, model.TimesPriceType, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire := `{"model":"gpt-6-luna","answers":[{"type":"predicate","name":"color","probability":1}],"usage":{"input_tokens":2048,"input_tokens_details":{` + tc.cache + `},` + tc.output + `}}`
			var calls atomic.Int32
			fixture := newLongLivedRouteFixtureWithHTTP(t, false, true, func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, wire)
			})
			configureDecisionsRouteTest(t, fixture, alias, tc.priceType, tc.outputPrice)

			status, _, delivered := postDecision(t, fixture, fixture.token, body)
			if status != http.StatusOK || delivered != wire || calls.Load() != 1 {
				t.Fatalf("结算观察改变了交付：HTTP %d，上游调用 %d 次，正文 %q", status, calls.Load(), delivered)
			}
			var logs []model.Log
			if err := fixture.db.Where("type = ? AND user_id = ?", model.LogTypeConsume, fixture.userID).Find(&logs).Error; err != nil {
				t.Fatal(err)
			}
			if tc.charge == 0 {
				if len(logs) != 0 {
					t.Fatalf("不可计价的用量产生了 %d 条消费记录，首条费用为 %d", len(logs), logs[0].Quota)
				}
			} else if len(logs) != 1 {
				t.Fatalf("应当生成一条消费记录，实际 %d 条", len(logs))
			} else if logs[0].Quota != tc.charge {
				t.Fatalf("消费费用 %d，预期 %d", logs[0].Quota, tc.charge)
			}
			fixture.awaitQuota(t, tc.charge)
		})
	}
}

func configureDecisionsRouteTest(t *testing.T, fixture *realtimeRouteFixture, alias, priceType string, outputPrice float64) {
	t.Helper()
	var channel model.Channel
	if err := fixture.db.First(&channel).Error; err != nil {
		t.Fatal(err)
	}
	mapping := `{"decisions-luna":"+gpt-6-luna"}`
	if err := fixture.db.Model(&channel).Updates(map[string]any{"models": alias, "model_mapping": mapping}).Error; err != nil {
		t.Fatal(err)
	}
	high, low := int64(100), int64(-100)
	for _, candidate := range []*model.Channel{
		{Type: config.ChannelTypeAzureV1, Priority: &high, Key: "unsupported"},
		{Type: config.ChannelTypeOpenAI, Priority: &low, Key: "fallback"},
	} {
		candidate.BaseURL = channel.BaseURL
		candidate.Group = channel.Group
		candidate.Models = alias
		candidate.ModelMapping = &mapping
		candidate.Status = config.ChannelStatusEnabled
		if err := fixture.db.Create(candidate).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := model.ChannelGroup.Load(); err != nil {
		t.Fatal(err)
	}
	extras := datatypes.NewJSONType(map[string]float64{config.UsageExtraCache: 0.25, config.UsageExtraCacheWrite: 1.5, config.UsageExtraReasoning: 4})
	price := &model.Price{Model: alias, Type: priceType, Input: 2, Output: outputPrice, ExtraRatios: &extras}
	if err := fixture.db.Create(price).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := model.BumpPublicationVersionCAS(t.Context(), fixture.db, model.PublicationOwnerPrice, model.PricingInstance.PublishedVersion()); err != nil {
		t.Fatal(err)
	}
	if err := model.PricingInstance.Init(); err != nil {
		t.Fatal(err)
	}
	model.GlobalUserGroupRatio.Lock()
	model.GlobalUserGroupRatio.APILimiter[channel.Group] = ratelimit.NewMemoryLimiter(100, 100, time.Minute, false)
	model.GlobalUserGroupRatio.Unlock()
	oldLog, oldRetry := config.LogConsumeEnabled, config.RetryTimes
	config.LogConsumeEnabled, config.RetryTimes = true, 2
	t.Cleanup(func() { config.LogConsumeEnabled, config.RetryTimes = oldLog, oldRetry })
}

func postDecision(t *testing.T, fixture *realtimeRouteFixture, credential, body string) (int, http.Header, string) {
	t.Helper()
	baseURL := "http" + strings.TrimPrefix(strings.TrimSuffix(fixture.url, "/v1/responses"), "ws")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/decisions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if credential != "" {
		req.Header.Set("Authorization", "Bearer "+credential)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, string(payload)
}
