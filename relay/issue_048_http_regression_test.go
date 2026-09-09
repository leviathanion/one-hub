package relay

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"one-api/common/config"
	"one-api/common/groupctx"
	"one-api/common/requester"
	commonresponses "one-api/common/responses"
	"one-api/model"
	"one-api/providers/openai"
	"one-api/types"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

const issue048HTTPModel = "gpt-5.6"

type issue048HTTPCase struct {
	name        string
	body        string
	wantCharge  int
	wantSearch  bool
	wantImage   bool
	wantFailure bool
}

func TestIssue048ResponsesHTTPToolEvidenceReachesSQL(t *testing.T) {
	for _, testCase := range []issue048HTTPCase{
		{
			name:        "search done then provider EOF",
			wantCharge:  5000,
			wantSearch:  true,
			wantFailure: true,
			body: "data: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_i048_search_eof\",\"model\":\"gpt-5.6\",\"status\":\"in_progress\",\"tools\":[{\"type\":\"web_search_preview\",\"search_context_size\":\"medium\"}]}}\n\n" +
				"data: {\"type\":\"response.output_item.done\",\"sequence_number\":1,\"output_index\":0,\"item\":{\"id\":\"ws_i048_search_eof\",\"type\":\"web_search_call\",\"status\":\"completed\",\"action\":{\"type\":\"search\"}}}\n\n",
		},
		{
			name:       "search successful terminal without token usage",
			wantCharge: 5000,
			wantSearch: true,
			body: "data: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_i048_search_done\",\"model\":\"gpt-5.6\",\"status\":\"in_progress\",\"tools\":[{\"type\":\"web_search_preview\",\"search_context_size\":\"medium\"}]}}\n\n" +
				"data: {\"type\":\"response.output_item.done\",\"sequence_number\":1,\"output_index\":0,\"item\":{\"id\":\"ws_i048_search_done\",\"type\":\"web_search_call\",\"status\":\"completed\",\"action\":{\"type\":\"search\"}}}\n\n" +
				"data: {\"type\":\"response.output_item.done\",\"sequence_number\":2,\"output_index\":0,\"item\":{\"id\":\"ws_i048_search_done\",\"type\":\"web_search_call\",\"status\":\"completed\",\"action\":{\"type\":\"search\"}}}\n\n" +
				"data: {\"type\":\"response.completed\",\"sequence_number\":3,\"response\":{\"id\":\"resp_i048_search_done\",\"model\":\"gpt-5.6\",\"status\":\"completed\",\"output\":[{\"id\":\"ws_i048_search_done\",\"type\":\"web_search_call\",\"status\":\"completed\",\"action\":{\"type\":\"search\"}}]}}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name:       "image successful terminal without token usage",
			wantCharge: 5500,
			wantImage:  true,
			body: "data: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_i048_image_done\",\"model\":\"gpt-5.6\",\"status\":\"in_progress\",\"tools\":[{\"type\":\"image_generation\",\"model\":\"gpt-image-1-mini\",\"quality\":\"medium\",\"size\":\"1024x1024\"}]}}\n\n" +
				"data: {\"type\":\"response.output_item.done\",\"sequence_number\":1,\"output_index\":0,\"item\":{\"id\":\"img_i048_done\",\"type\":\"image_generation_call\",\"status\":\"completed\",\"quality\":\"medium\",\"size\":\"1024x1024\"}}\n\n" +
				"data: {\"type\":\"response.completed\",\"sequence_number\":2,\"response\":{\"id\":\"resp_i048_image_done\",\"model\":\"gpt-5.6\",\"status\":\"completed\",\"tools\":[{\"type\":\"image_generation\",\"model\":\"gpt-image-1-mini\",\"quality\":\"medium\",\"size\":\"1024x1024\"}],\"output\":[{\"id\":\"img_i048_done\",\"type\":\"image_generation_call\",\"status\":\"completed\",\"quality\":\"medium\",\"size\":\"1024x1024\"}]}}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name:        "image output then provider EOF",
			wantFailure: true,
			body: "data: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_i048_image_eof\",\"model\":\"gpt-5.6\",\"status\":\"in_progress\",\"tools\":[{\"type\":\"image_generation\",\"model\":\"gpt-image-1-mini\",\"quality\":\"medium\",\"size\":\"1024x1024\"}]}}\n\n" +
				"data: {\"type\":\"response.output_item.done\",\"sequence_number\":1,\"output_index\":0,\"item\":{\"id\":\"img_i048_eof\",\"type\":\"image_generation_call\",\"status\":\"completed\",\"quality\":\"medium\",\"size\":\"1024x1024\"}}\n\n",
		},
		{
			name:        "image tool declaration then provider EOF",
			wantFailure: true,
			body:        "data: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_i048_image_declared\",\"model\":\"gpt-5.6\",\"status\":\"in_progress\",\"tools\":[{\"type\":\"image_generation\",\"model\":\"gpt-image-1-mini\",\"quality\":\"medium\",\"size\":\"1024x1024\"}]}}\n\n",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			issue048RunHTTPSettlement(t, testCase)
		})
	}
}

func issue048RunHTTPSettlement(t *testing.T, testCase issue048HTTPCase) {
	t.Helper()
	setupRelayTestDB(t, &model.User{}, &model.Token{}, &model.Log{})
	if err := model.DB.Create(&model.User{
		Id: 1, Username: "i048-user", Password: "password123", AccessToken: "i048-access",
		Quota: 100000, Group: "default", Status: config.UserStatusEnabled, Role: config.RoleCommonUser,
	}).Error; err != nil {
		t.Fatalf("create I048 user fixture: %v", err)
	}
	if err := model.DB.Session(&gorm.Session{SkipHooks: true}).Create(&model.Token{
		Id: 1, UserId: 1, Key: "i048-token", Name: "i048-token", Status: config.TokenStatusEnabled,
		ExpiredTime: -1, RemainQuota: 100000, Group: "default",
	}).Error; err != nil {
		t.Fatalf("create I048 token fixture: %v", err)
	}

	originalPricing := model.PricingInstance
	originalBatch := config.BatchUpdateEnabled
	originalRedis := config.RedisEnabled
	originalReserve := config.PreConsumedQuota
	originalLog := config.LogConsumeEnabled
	originalDisableTokenEncoders := config.DisableTokenEncoders
	model.PricingInstance = &model.Pricing{Prices: map[string]*model.Price{
		issue048HTTPModel: {Model: issue048HTTPModel, Type: model.TokensPriceType},
	}}
	config.BatchUpdateEnabled = false
	config.RedisEnabled = false
	config.PreConsumedQuota = 0
	config.LogConsumeEnabled = true
	config.DisableTokenEncoders = true
	t.Cleanup(func() {
		model.PricingInstance = originalPricing
		config.BatchUpdateEnabled = originalBatch
		config.RedisEnabled = originalRedis
		config.PreConsumedQuota = originalReserve
		config.LogConsumeEnabled = originalLog
		config.DisableTokenEncoders = originalDisableTokenEncoders
	})

	var upstreamCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, testCase.body)
	}))
	t.Cleanup(server.Close)
	previousClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = previousClient })

	rawRequest := `{"model":"` + issue048HTTPModel + `","input":"hello","stream":true,"store":false}`
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses?stream=true", strings.NewReader(rawRequest)).WithContext(context.Background())
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Set("id", 1)
	ctx.Set("token_id", 1)
	ctx.Set("group_ratio", 1.0)
	groupctx.SetRoutingGroup(ctx, "default", groupctx.RoutingGroupSourceUserGroup)

	proxy := ""
	baseURL := server.URL
	provider := openai.CreateOpenAIProvider(&model.Channel{Plugin: model.NewCustomEndpointPlugin(),
		Type: config.ChannelTypeCustom, Key: "i048-provider-key", Proxy: &proxy, BaseURL: &baseURL,
	}, server.URL)
	provider.SetContext(ctx)

	envelope, err := commonresponses.ParseRawEnvelope([]byte(rawRequest))
	if err != nil {
		t.Fatalf("parse I048 Responses request: %v", err)
	}
	relay := &relayResponses{
		relayBase: relayBase{
			c:             ctx,
			provider:      provider,
			originalModel: issue048HTTPModel,
			modelName:     issue048HTTPModel,
		},
		responsesRequest: envelope.Projection,
		rawEnvelope:      envelope,
		operation:        responsesOperationCreate,
	}

	apiErr, _ := RelayHandler(relay)
	if testCase.wantFailure {
		if apiErr == nil {
			t.Fatal("provider EOF without terminal unexpectedly succeeded")
		}
	} else if apiErr != nil {
		t.Fatalf("Responses stream failed: %+v", apiErr)
	}
	if upstreamCalls.Load() != 1 {
		t.Fatalf("expected one real upstream HTTP request, got %d", upstreamCalls.Load())
	}
	if recorder.Code == 0 {
		t.Fatal("relay did not write an HTTP response")
	}

	usage := provider.GetUsage()
	searchKey := types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "medium")
	imageKey := types.BuildExtraBillingKey(types.APIToolTypeImageGeneration, "gpt-image-1-mini|medium|1024x1024|0")
	if testCase.wantSearch {
		if usage.ExtraBilling[searchKey].CallCount != 1 || !usage.HasProviderExtraBilling(searchKey) {
			t.Fatalf("search evidence did not survive HTTP stream: %+v", usage)
		}
		if _, exists := usage.ExtraBilling[imageKey]; exists {
			t.Fatalf("search request unexpectedly produced image billing: %+v", usage.ExtraBilling)
		}
	} else if testCase.wantImage {
		if usage.ExtraBilling[imageKey].CallCount != 1 || !usage.HasProviderExtraBilling(imageKey) {
			t.Fatalf("image evidence did not survive successful terminal: %+v", usage)
		}
		if _, exists := usage.ExtraBilling[searchKey]; exists {
			t.Fatalf("image request unexpectedly produced search billing: %+v", usage.ExtraBilling)
		}
	} else if len(usage.ExtraBilling) != 0 || len(usage.ProviderExtraBilling) != 0 {
		t.Fatalf("incomplete image stream produced billing: %+v", usage)
	}
	issue048AssertSQLSettlement(t, testCase.wantCharge)
}

func issue048AssertSQLSettlement(t *testing.T, charge int) {
	t.Helper()
	var user model.User
	if err := model.DB.First(&user, 1).Error; err != nil {
		t.Fatalf("read I048 user settlement: %v", err)
	}
	var token model.Token
	if err := model.DB.First(&token, 1).Error; err != nil {
		t.Fatalf("read I048 token settlement: %v", err)
	}
	wantRequests := 0
	if charge > 0 {
		wantRequests = 1
	}
	if user.Quota != 100000-charge || token.RemainQuota != 100000-charge || user.UsedQuota != charge || token.UsedQuota != charge || user.RequestCount != wantRequests {
		t.Fatalf("I048 SQL balances mismatch: user=%+v token=%+v charge=%d", user, token, charge)
	}
	var logs []model.Log
	if err := model.DB.Where("type = ?", model.LogTypeConsume).Find(&logs).Error; err != nil {
		t.Fatalf("read I048 consume logs: %v", err)
	}
	if len(logs) != wantRequests || len(logs) == 1 && logs[0].Quota != charge {
		t.Fatalf("I048 SQL consume log mismatch: logs=%+v charge=%d", logs, charge)
	}
}
