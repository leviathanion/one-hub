package relay

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"one-api/common/config"
	"one-api/common/groupctx"
	"one-api/common/requester"
	"one-api/model"
	"one-api/providers/openai"
	relayUtil "one-api/relay/relay_util"
	"one-api/types"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func TestFixI024CompatibleSendKeepsChatContentWhenProviderOmitsUsage(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		usage       string
		expectUsage bool
	}{
		{name: "usage omitted"},
		{name: "usage null", usage: `,"usage":null`},
		{name: "complete usage", usage: `,"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}`, expectUsage: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			setupRelayTestDB(t, &model.User{}, &model.Token{})
			if err := model.DB.Create(&model.User{
				Id:          1,
				Username:    "i024-user",
				Password:    "password123",
				AccessToken: "i024-access-token",
				Quota:       100000,
				Group:       "default",
				Status:      config.UserStatusEnabled,
				Role:        config.RoleCommonUser,
				DisplayName: "I024 User",
				CreatedTime: 1,
			}).Error; err != nil {
				t.Fatalf("create billing user fixture: %v", err)
			}
			if err := model.DB.Session(&gorm.Session{SkipHooks: true}).Create(&model.Token{
				Id:          1,
				UserId:      1,
				Key:         "i024-token",
				Name:        "i024-token",
				RemainQuota: 100000,
				Group:       "default",
			}).Error; err != nil {
				t.Fatalf("create billing token fixture: %v", err)
			}
			originalPricing := model.PricingInstance
			originalBatchUpdate := config.BatchUpdateEnabled
			originalRedisEnabled := config.RedisEnabled
			originalPreConsumedQuota := config.PreConsumedQuota
			originalLogConsume := config.LogConsumeEnabled
			model.PricingInstance = &model.Pricing{Prices: map[string]*model.Price{
				"gpt-5": {Model: "gpt-5", Type: model.TokensPriceType, Input: 1, Output: 1},
			}}
			config.BatchUpdateEnabled = false
			config.RedisEnabled = false
			config.PreConsumedQuota = 50
			config.LogConsumeEnabled = false
			t.Cleanup(func() {
				model.PricingInstance = originalPricing
				config.BatchUpdateEnabled = originalBatchUpdate
				config.RedisEnabled = originalRedisEnabled
				config.PreConsumedQuota = originalPreConsumedQuota
				config.LogConsumeEnabled = originalLogConsume
			})

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
					http.Error(w, "unexpected request", http.StatusNotFound)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				body := `{"id":"chatcmpl_i024","object":"chat.completion","model":"gpt-5","choices":[{"index":0,"message":{"role":"assistant","content":"hello","tool_calls":[{"id":"call_i024","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"ok\"}"}}]},"finish_reason":"tool_calls"}]` + testCase.usage + `}`
				_, _ = io.WriteString(w, body)
			}))
			t.Cleanup(server.Close)

			previousClient := requester.HTTPClient
			requester.HTTPClient = server.Client()
			t.Cleanup(func() { requester.HTTPClient = previousClient })

			proxy := ""
			channel := &model.Channel{Plugin: model.NewCustomEndpointPlugin(), Type: config.ChannelTypeCustom, Key: "test-key", BaseURL: i024StringPointer(server.URL), Proxy: &proxy}
			provider := openai.CreateOpenAIProvider(channel, server.URL)
			provider.SetUsage(&types.Usage{})

			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(context.Background())
			ctx.Set("id", 1)
			ctx.Set("token_id", 1)
			ctx.Set("group_ratio", 1.0)
			groupctx.SetRoutingGroup(ctx, "default", groupctx.RoutingGroupSourceUserGroup)
			store := false
			chatRequest := &types.ChatCompletionRequest{
				Model:    "gpt-5",
				Messages: []types.ChatCompletionMessage{{Role: types.ChatMessageRoleUser, Content: "question"}},
			}
			relay := &relayResponses{
				relayBase: relayBase{c: ctx, provider: provider, modelName: "gpt-5"},
				responsesRequest: types.OpenAIResponsesRequest{
					Model: "gpt-5",
					Input: "question",
					Store: &store,
				},
				preparedChatRequest: chatRequest,
			}

			apiErr, done := relay.compatibleSend(provider)
			if apiErr != nil || done {
				t.Fatalf("compatible Chat→Responses send failed: err=%+v done=%v", apiErr, done)
			}
			if recorder.Code != http.StatusOK {
				t.Fatalf("expected HTTP success, got %d: %s", recorder.Code, recorder.Body.String())
			}

			var response types.OpenAIResponsesResponses
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatalf("client could not parse Responses JSON: %v; body=%s", err, recorder.Body.String())
			}
			if len(response.Output) != 2 {
				t.Fatalf("expected assistant text and tool call, got %#v", response.Output)
			}
			if response.Output[0].StringContent() != "hello" {
				t.Fatalf("assistant text was lost during compatibility conversion: %#v", response.Output[0])
			}
			tool := response.Output[1]
			if tool.Type != types.InputTypeFunctionCall || tool.CallID != "call_i024" || tool.Name != "lookup" || tool.Arguments == nil || *tool.Arguments != `{"q":"ok"}` {
				t.Fatalf("tool result was lost during compatibility conversion: %#v", tool)
			}

			if testCase.expectUsage {
				if response.Usage == nil || response.Usage.InputTokens != 11 || response.Usage.OutputTokens != 7 || response.Usage.TotalTokens != 18 {
					t.Fatalf("complete provider usage was not converted: %+v", response.Usage)
				}
				if !provider.GetUsage().HasProviderUsage() {
					t.Fatalf("complete provider usage was not retained as billing evidence: %+v", provider.GetUsage())
				}
			} else {
				if response.Usage != nil || provider.GetUsage().HasProviderUsage() {
					t.Fatalf("missing provider usage became a token charge: response=%+v provider=%+v", response.Usage, provider.GetUsage())
				}
				var public map[string]json.RawMessage
				if err := json.Unmarshal(recorder.Body.Bytes(), &public); err != nil {
					t.Fatalf("parse public Responses object: %v", err)
				}
				if _, present := public["usage"]; present {
					t.Fatalf("missing provider usage must not be declared as zero usage: %s", recorder.Body.String())
				}
			}

			attempt, err := relayUtil.NewAttemptQuota(ctx, "gpt-5", 0, relayUtil.BillingAttemptSpec{})
			if err != nil {
				t.Fatalf("create billing attempt: %v", err)
			}
			if err := attempt.ApplyReserve(ctx.Request.Context()); err != nil {
				t.Fatalf("reserve billing quota: %v", err)
			}
			if err := attempt.ClaimSubmission(); err != nil {
				t.Fatalf("claim provider submission: %v", err)
			}
			settlement, err := attempt.CloseFromProviderResult(ctx.Request.Context(), provider.GetUsage(), false)
			if err != nil {
				t.Fatalf("settle provider usage: %v", err)
			}
			wantCharge := int64(0)
			wantQuota := 100000
			if testCase.expectUsage {
				wantCharge = 18
				wantQuota -= int(wantCharge)
				if !settlement.Confirmed || settlement.ChargedQuota != wantCharge {
					t.Fatalf("complete provider usage was not charged through Attempt: %+v", settlement)
				}
			} else if settlement.Confirmed || settlement.ChargedQuota != 0 {
				t.Fatalf("missing provider usage was charged through Attempt: %+v", settlement)
			}
			var user model.User
			if err := model.DB.First(&user, 1).Error; err != nil {
				t.Fatalf("read settled user: %v", err)
			}
			var token model.Token
			if err := model.DB.First(&token, 1).Error; err != nil {
				t.Fatalf("read settled token: %v", err)
			}
			if user.Quota != wantQuota || token.RemainQuota != wantQuota || int64(user.UsedQuota) != wantCharge || int64(token.UsedQuota) != wantCharge {
				t.Fatalf("unexpected SQL settlement: user=%+v token=%+v want_quota=%d want_charge=%d", user, token, wantQuota, wantCharge)
			}
		})
	}
}

func i024StringPointer(value string) *string {
	return &value
}
