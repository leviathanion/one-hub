package relay_util

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"one-api/common/utils"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"one-api/common"
	"one-api/common/config"
	"one-api/common/groupctx"
	"one-api/common/requester"
	"one-api/common/wsconn"
	"one-api/common/wsconn/wstest"
	"one-api/model"
	"one-api/providers/azure"
	"one-api/providers/azure_v1"
	"one-api/providers/base"
	"one-api/providers/openai"
	"one-api/providers/siliconflow"
	runtimerealtime "one-api/runtime/realtime"
	runtimesession "one-api/runtime/session"
	"one-api/types"

	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"
)

func TestIssue054HTTPDurationIsPublishedByAllSharedFactories(t *testing.T) {
	for _, test := range issue054HTTPFactories() {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"text":"ok","model":"i054-duration","usage":{"type":"duration","seconds":9}}`)
			}))
			t.Cleanup(server.Close)
			previousClient := requester.HTTPClient
			requester.HTTPClient = server.Client()
			t.Cleanup(func() { requester.HTTPClient = previousClient })

			proxy, baseURL := "", server.URL
			channel := &model.Channel{Plugin: model.NewCustomEndpointPlugin(), Type: test.channel, Key: "i054-key", Proxy: &proxy, BaseURL: &baseURL}
			if test.azureAPI != "" {
				channel.Other = `{"api_version":"` + test.azureAPI + `"}`
			}
			provider := test.newProvider(channel, server.URL)
			provider.SetContext(issue054TranscriptionContext(t, false))
			provider.SetOriginalModel("i054-duration")
			provider.SetUsage(&types.Usage{})
			transcriber, ok := provider.(base.TranscriptionsInterface)
			if !ok {
				t.Fatalf("factory lost shared transcription interface: %T", provider)
			}
			response, apiErr := transcriber.CreateTranscriptions(&types.AudioRequest{Model: "i054-duration", ResponseFormat: "json"})
			if apiErr != nil || response == nil {
				t.Fatalf("duration HTTP transcription failed: response=%+v err=%+v", response, apiErr)
			}
			usage := provider.GetUsage()
			if usage == nil || usage.ProviderOperationUnits == nil || *usage.ProviderOperationUnits != 1 ||
				!usage.ProviderIndependentUsageUnits[config.UsageExtraInputAudioTranscription] || usage.ExtraUsageUnits[config.UsageExtraInputAudioTranscription] != 9 {
				t.Fatalf("factory did not publish duration operation evidence: %+v", usage)
			}
		})
	}
}

func TestIssue054HTTPFactoriesCarryDurationFromProducerToSQL(t *testing.T) {
	for _, factory := range issue054HTTPFactories() {
		t.Run(factory.name, func(t *testing.T) {
			for _, priceCase := range []struct {
				name       string
				priceType  string
				extraRatio *float64
				rateRules  bool
				want       int64
			}{
				{name: "times base", priceType: model.TimesPriceType, want: 1000},
				{name: "times with tier and long-context rules", priceType: model.TimesPriceType, rateRules: true, want: 1000},
				{name: "times plus duration", priceType: model.TimesPriceType, extraRatio: float64PtrIssue054(1), want: 1009},
				{name: "tokens plus duration", priceType: model.TokensPriceType, extraRatio: float64PtrIssue054(1), want: 9},
			} {
				t.Run(priceCase.name, func(t *testing.T) {
					c := realtimeBillingFixture(t)
					oldPricing, oldBatch, oldRedis, oldLog, oldReserve := model.PricingInstance, config.BatchUpdateEnabled, config.RedisEnabled, config.LogConsumeEnabled, config.PreConsumedQuota
					price := issue054Price(priceCase.priceType, priceCase.extraRatio)
					if priceCase.rateRules {
						price.RateRules = issue054RateRules()
					}
					model.PricingInstance = &model.Pricing{Prices: map[string]*model.Price{
						"i054-duration": price,
					}}
					config.BatchUpdateEnabled = false
					config.RedisEnabled = false
					config.LogConsumeEnabled = true
					config.PreConsumedQuota = 50
					t.Cleanup(func() {
						model.PricingInstance = oldPricing
						config.BatchUpdateEnabled = oldBatch
						config.RedisEnabled = oldRedis
						config.LogConsumeEnabled = oldLog
						config.PreConsumedQuota = oldReserve
					})

					var requests int
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						requests++
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, `{"text":"ok","model":"i054-duration","usage":{"type":"duration","seconds":9}}`)
					}))
					t.Cleanup(server.Close)
					oldHTTPClient := requester.HTTPClient
					requester.HTTPClient = server.Client()
					t.Cleanup(func() { requester.HTTPClient = oldHTTPClient })
					proxy, baseURL := "", server.URL
					channel := &model.Channel{Plugin: model.NewCustomEndpointPlugin(), Type: factory.channel, Key: "i054-key", Proxy: &proxy, BaseURL: &baseURL}
					if factory.azureAPI != "" {
						channel.Other = `{"api_version":"` + factory.azureAPI + `"}`
					}
					provider := factory.newProvider(channel, server.URL)
					provider.SetContext(issue054TranscriptionContext(t, false))
					provider.SetOriginalModel("i054-duration")
					provider.SetUsage(&types.Usage{})
					transcriber, ok := provider.(base.TranscriptionsInterface)
					if !ok {
						t.Fatalf("factory lost shared transcription interface: %T", provider)
					}
					response, apiErr := transcriber.CreateTranscriptions(&types.AudioRequest{Model: "i054-duration", ResponseFormat: "json"})
					if apiErr != nil || response == nil {
						t.Fatalf("duration producer failed: response=%+v err=%+v", response, apiErr)
					}
					if requests != 1 {
						t.Fatalf("duration attempt was replayed: requests=%d", requests)
					}
					usage := provider.GetUsage()
					if usage == nil || usage.ProviderOperationUnits == nil || *usage.ProviderOperationUnits != 1 {
						t.Fatalf("producer did not publish operation evidence: %+v", usage)
					}
					issue054SettleHTTPUsageSQL(t, c, usage, priceCase.want)
				})
			}
		})
	}
}

func TestIssue054RealtimeFactoriesCarryDurationToOwnSQLObserver(t *testing.T) {
	for _, factory := range issue054RealtimeFactories() {
		t.Run(factory.name, func(t *testing.T) {
			c := realtimeBillingFixture(t)
			oldPricing, oldBatch, oldRedis, oldLog, oldReserve := model.PricingInstance, config.BatchUpdateEnabled, config.RedisEnabled, config.LogConsumeEnabled, config.PreConsumedQuota
			model.PricingInstance = &model.Pricing{Prices: map[string]*model.Price{
				"i054-realtime": issue054Price(model.TimesPriceType, nil),
			}}
			config.BatchUpdateEnabled = false
			config.RedisEnabled = false
			config.LogConsumeEnabled = true
			config.PreConsumedQuota = 50
			if err := model.DB.Model(&model.User{}).Where("id = ?", 1).Update("quota", 3000).Error; err != nil {
				t.Fatal(err)
			}
			if err := model.DB.Model(&model.Token{}).Where("id = ?", 1).Update("remain_quota", 3000).Error; err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				model.PricingInstance = oldPricing
				config.BatchUpdateEnabled = oldBatch
				config.RedisEnabled = oldRedis
				config.LogConsumeEnabled = oldLog
				config.PreConsumedQuota = oldReserve
			})

			upstreamURL, cleanup := issue054RealtimeUpstream(t)
			t.Cleanup(cleanup)
			httpBaseURL := "http" + strings.TrimPrefix(upstreamURL, "ws")
			proxy := ""
			channel := &model.Channel{Plugin: model.NewCustomEndpointPlugin(), Type: factory.channel, Key: "i054-key", Proxy: &proxy}
			channel.BaseURL = &httpBaseURL
			if factory.azureAPI != "" {
				channel.Other = `{"api_version":"` + factory.azureAPI + `","self_hosted":true}`
			} else {
				channel.Other = `{"self_hosted":true}`
			}
			provider := factory.newProvider(channel, httpBaseURL)
			provider.SetContext(c)
			models := runtimesession.ModelBinding{RequestedModel: "i054-realtime", ProviderModel: "i054-realtime", BillingModel: "i054-realtime"}
			openOptions := runtimerealtime.RealtimeOpenOptions{Models: models, Context: context.Background()}
			realtimeProvider, ok := provider.(base.RealtimeSessionProviderWithOptions)
			if !ok {
				t.Fatalf("factory lost shared Realtime session interface: %T", provider)
			}
			session, apiErr := realtimeProvider.OpenRealtimeSessionWithOptions("i054-realtime", openOptions)
			if apiErr != nil || session == nil {
				t.Fatalf("Realtime session open failed: session=%T err=%+v", session, apiErr)
			}
			t.Cleanup(func() { session.Abort("test_cleanup") })
			session.SetTurnObserverFactory(NewRealtimeTurnObserverFactory(c, models, nil))

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			events := make(chan runtimerealtime.RecvEvent, 16)
			errorsCh := make(chan error, 1)
			receiverDone := make(chan struct{})
			go func() {
				defer close(receiverDone)
				for {
					event, err := session.Recv(ctx)
					if err != nil {
						errorsCh <- err
						return
					}
					events <- event
				}
			}()
			defer func() {
				session.Abort("test_done")
				cancel()
				select {
				case <-receiverDone:
				case <-time.After(time.Second):
					t.Error("timed out waiting for Realtime receiver to stop")
				}
			}()
			if err := session.SendClient(ctx, runtimerealtime.NewTextFrame([]byte(`{"type":"session.update","session":{"turn_detection":null,"input_audio_transcription":{"model":"i054-realtime"}}}`))); err != nil {
				t.Fatalf("send transcription configuration: %v", err)
			}
			completed := 0
			waitForCompletions := func(want int) {
				t.Helper()
				for completed < want {
					select {
					case event := <-events:
						if event.Frame == nil {
							continue
						}
						var envelope struct {
							Type string `json:"type"`
						}
						if json.Unmarshal(event.Frame.Payload(), &envelope) == nil && envelope.Type == types.EventTypeInputAudioTranscriptionCompleted {
							completed++
						}
					case err := <-errorsCh:
						t.Fatalf("Realtime receive failed before duration completions: %v", err)
					case <-ctx.Done():
						t.Fatalf("timed out waiting for duration completions: %v", ctx.Err())
					}
				}
			}
			for index, want := range []int{3, 5} {
				if err := session.SendClient(ctx, runtimerealtime.NewTextFrame([]byte(`{"type":"input_audio_buffer.commit"}`))); err != nil {
					t.Fatalf("send audio commit %d: %v", index+1, err)
				}
				waitForCompletions(want)
			}
			var user model.User
			var token model.Token
			if err := model.DB.First(&user, 1).Error; err != nil {
				t.Fatal(err)
			}
			if err := model.DB.First(&token, 1).Error; err != nil {
				t.Fatal(err)
			}
			if user.Quota != 1000 || token.RemainQuota != 1000 || user.UsedQuota != 2000 || token.UsedQuota != 2000 {
				t.Fatalf("Realtime factory owners did not each settle one duration operation: user=%+v token=%+v", user, token)
			}
			var logs []model.Log
			if err := model.DB.Where("type = ?", model.LogTypeConsume).Find(&logs).Error; err != nil {
				t.Fatal(err)
			}
			if len(logs) != 2 || logs[0].Quota != 1000 || logs[1].Quota != 1000 {
				t.Fatalf("Realtime factory SQL audit did not contain exactly two 1000 charges: %+v", logs)
			}
		})
	}
}

func TestIssue054DurationComponentsUseOperationAndIndependentPricesTogether(t *testing.T) {
	usageEvent := &types.UsageEvent{
		Source:          types.UsageSourceInputAudioTranscription,
		BillingBasis:    types.UsageBillingBasisDuration,
		DurationSeconds: 9,
	}
	usageEvent.SetProviderIndependentUsageUnit(config.UsageExtraInputAudioTranscription, 9)
	usageEvent.MarkProviderOperationUnits(1)
	projected := ProviderUsageEventForBilling(usageEvent)
	if projected == nil || projected.ProviderOperationUnits == nil || *projected.ProviderOperationUnits != 1 {
		t.Fatalf("billing projection lost operation evidence: %+v", projected)
	}

	for _, test := range []struct {
		name       string
		priceType  string
		extraRatio *float64
		want       int64
	}{
		{name: "times base only", priceType: model.TimesPriceType, want: 1000},
		{name: "times plus explicit duration", priceType: model.TimesPriceType, extraRatio: float64PtrIssue054(1), want: 1009},
		{name: "tokens plus explicit duration", priceType: model.TokensPriceType, extraRatio: float64PtrIssue054(1), want: 9},
	} {
		t.Run(test.name, func(t *testing.T) {
			pricing := &model.Pricing{Prices: map[string]*model.Price{
				"i054-price": issue054Price(test.priceType, test.extraRatio),
			}}
			quota := issue005Quota(t, pricing, "i054-price")
			decision := quota.EvaluateProviderUsage(projected.ToChatUsage())
			if !decision.Confirm || decision.FinalQuota != test.want {
				t.Fatalf("duration components were reduced incorrectly: decision=%+v want=%d", decision, test.want)
			}
		})
	}

	// RateRules apply to token prices only.  A times operation remains one
	// configured call even when a service tier and long-context rule exist.
	rules := issue054RateRules()
	timesWithRules := issue054Price(model.TimesPriceType, nil)
	timesWithRules.RateRules = rules
	pricing := &model.Pricing{Prices: map[string]*model.Price{"i054-rate-rules": timesWithRules}}
	quota := issue005Quota(t, pricing, "i054-rate-rules")
	timesUsage := projected.ToChatUsage()
	timesUsage.PromptTokens = 2
	timesUsage.ServiceTier = "flex"
	decision := quota.EvaluateProviderUsage(timesUsage)
	if !decision.Confirm || decision.FinalQuota != 1000 {
		t.Fatalf("service-tier/long-context token rules changed fixed duration operation: decision=%+v", decision)
	}
}

func TestIssue054HTTPDurationChargeRefundAndLogAreIdempotent(t *testing.T) {
	c := realtimeBillingFixture(t)
	oldPricing, oldBatch, oldRedis, oldLog, oldReserve := model.PricingInstance, config.BatchUpdateEnabled, config.RedisEnabled, config.LogConsumeEnabled, config.PreConsumedQuota
	model.PricingInstance = &model.Pricing{Prices: map[string]*model.Price{
		"i054-duration": issue054Price(model.TimesPriceType, nil),
	}}
	config.BatchUpdateEnabled = false
	config.RedisEnabled = false
	config.LogConsumeEnabled = true
	config.PreConsumedQuota = 50
	t.Cleanup(func() {
		model.PricingInstance = oldPricing
		config.BatchUpdateEnabled = oldBatch
		config.RedisEnabled = oldRedis
		config.LogConsumeEnabled = oldLog
		config.PreConsumedQuota = oldReserve
	})

	ctx := c
	attempt, err := NewAttemptQuota(ctx, "i054-duration", 0, BillingAttemptSpec{LogProtocol: LogProtocolHTTP})
	if err != nil {
		t.Fatal(err)
	}
	if err := attempt.ApplyReserve(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := attempt.ClaimSubmission(); err != nil {
		t.Fatal(err)
	}
	usage := (&types.UsageEvent{
		Source:          types.UsageSourceInputAudioTranscription,
		BillingBasis:    types.UsageBillingBasisDuration,
		DurationSeconds: 9,
	}).ToChatUsage()
	usage.SetProviderIndependentUsageUnit(config.UsageExtraInputAudioTranscription, 9)
	usage.MarkProviderOperationUnits(1)
	result, err := attempt.CloseFromProviderResult(context.Background(), usage, false)
	if err != nil || !result.Confirmed || result.Unsettled || result.ChargedQuota != 1000 {
		t.Fatalf("valid duration did not settle at the fixed operation price: result=%+v err=%v", result, err)
	}
	second, err := attempt.CloseFromProviderResult(context.Background(), usage, false)
	if err != nil || second != result {
		t.Fatalf("repeated close changed settled duration charge: first=%+v second=%+v err=%v", result, second, err)
	}

	var user model.User
	var token model.Token
	if err := model.DB.First(&user, 1).Error; err != nil {
		t.Fatal(err)
	}
	if err := model.DB.First(&token, 1).Error; err != nil {
		t.Fatal(err)
	}
	if user.Quota != 0 || token.RemainQuota != 0 || user.UsedQuota != 1000 || token.UsedQuota != 1000 {
		t.Fatalf("duration settlement changed SQL balances incorrectly: user=%+v token=%+v", user, token)
	}
	var logs []model.Log
	if err := model.DB.Where("type = ?", model.LogTypeConsume).Find(&logs).Error; err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 || logs[0].Quota != 1000 || logs[0].PromptTokens != 0 || logs[0].CompletionTokens != 0 {
		t.Fatalf("duration settlement audit log was duplicated or token-estimated: %+v", logs)
	}
}

func TestIssue054MissingOrInvalidDurationRefundsWithoutLog(t *testing.T) {
	for _, test := range []struct {
		name  string
		usage *types.Usage
	}{
		{name: "missing", usage: (&types.UsageEvent{Source: types.UsageSourceInputAudioTranscription, BillingBasis: types.UsageBillingBasisDuration}).ToChatUsage()},
		{name: "invalid", usage: (&types.UsageEvent{Source: types.UsageSourceInputAudioTranscription, BillingBasis: types.UsageBillingBasisDuration}).ToChatUsage()},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := realtimeBillingFixture(t)
			oldPricing, oldBatch, oldRedis, oldLog, oldReserve := model.PricingInstance, config.BatchUpdateEnabled, config.RedisEnabled, config.LogConsumeEnabled, config.PreConsumedQuota
			model.PricingInstance = &model.Pricing{Prices: map[string]*model.Price{"i054-refund": issue054Price(model.TimesPriceType, nil)}}
			config.BatchUpdateEnabled = false
			config.RedisEnabled = false
			config.LogConsumeEnabled = true
			config.PreConsumedQuota = 50
			t.Cleanup(func() {
				model.PricingInstance = oldPricing
				config.BatchUpdateEnabled = oldBatch
				config.RedisEnabled = oldRedis
				config.LogConsumeEnabled = oldLog
				config.PreConsumedQuota = oldReserve
			})
			attempt, err := NewAttemptQuota(c, "i054-refund", 0, BillingAttemptSpec{LogProtocol: LogProtocolHTTP})
			if err != nil {
				t.Fatal(err)
			}
			if err := attempt.ApplyReserve(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := attempt.ClaimSubmission(); err != nil {
				t.Fatal(err)
			}
			if test.name == "invalid" {
				bad := -1.0
				test.usage.SetProviderIndependentUsageUnit(config.UsageExtraInputAudioTranscription, bad)
				test.usage.MarkProviderOperationUnits(-1)
			}
			result, err := attempt.CloseFromProviderResult(context.Background(), test.usage, false)
			if err != nil || result.Confirmed || result.Unsettled || result.ChargedQuota != 0 {
				t.Fatalf("missing/invalid duration was charged: result=%+v err=%v usage=%+v", result, err, test.usage)
			}
			var user model.User
			var token model.Token
			if err := model.DB.First(&user, 1).Error; err != nil {
				t.Fatal(err)
			}
			if err := model.DB.First(&token, 1).Error; err != nil {
				t.Fatal(err)
			}
			if user.Quota != 1000 || token.RemainQuota != 1000 || user.UsedQuota != 0 || token.UsedQuota != 0 {
				t.Fatalf("refund changed SQL balances: user=%+v token=%+v", user, token)
			}
			var logs []model.Log
			if err := model.DB.Where("type = ?", model.LogTypeConsume).Find(&logs).Error; err != nil {
				t.Fatal(err)
			}
			if len(logs) != 0 {
				t.Fatalf("refunded duration wrote a consume log: %+v", logs)
			}
		})
	}
}

func TestIssue054RealtimeDurationOwnerRejectsWrongItemAndChargesEachWorkOnce(t *testing.T) {
	c := realtimeBillingFixture(t)
	oldPricing, oldBatch, oldRedis, oldLog, oldReserve := model.PricingInstance, config.BatchUpdateEnabled, config.RedisEnabled, config.LogConsumeEnabled, config.PreConsumedQuota
	model.PricingInstance = &model.Pricing{Prices: map[string]*model.Price{"i054-realtime": issue054Price(model.TimesPriceType, nil)}}
	config.BatchUpdateEnabled = false
	config.RedisEnabled = false
	config.LogConsumeEnabled = true
	config.PreConsumedQuota = 50
	if err := model.DB.Model(&model.User{}).Where("id = ?", 1).Update("quota", 3000).Error; err != nil {
		t.Fatal(err)
	}
	if err := model.DB.Model(&model.Token{}).Where("id = ?", 1).Update("remain_quota", 3000).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		model.PricingInstance = oldPricing
		config.BatchUpdateEnabled = oldBatch
		config.RedisEnabled = oldRedis
		config.LogConsumeEnabled = oldLog
		config.PreConsumedQuota = oldReserve
	})

	models := runtimesession.ModelBinding{RequestedModel: "i054-realtime", ProviderModel: "i054-realtime", BillingModel: "i054-realtime"}
	factory := NewRealtimeTurnObserverFactory(c, models, nil)
	wrong := factory().(*RealtimeTurnObserver)
	if err := wrong.AdmitBoundedTurn(runtimesession.TurnAdmission{Models: models, WorkID: "wrong-work", InputItemID: "item-a", Transcription: true}); err != nil {
		t.Fatal(err)
	}
	wrongUsage := &types.UsageEvent{Source: types.UsageSourceInputAudioTranscription, ItemID: "item-b", BillingBasis: types.UsageBillingBasisDuration}
	wrongUsage.SetProviderIndependentUsageUnit(config.UsageExtraInputAudioTranscription, 9)
	wrongUsage.MarkProviderOperationUnits(1)
	if err := wrong.ObserveTurnUsage(wrongUsage); err != nil {
		t.Fatal(err)
	}
	wrong.FinalizeTurn(runtimesession.TurnFinalizePayload{TerminationReason: "transcription.completed"})
	if got := wrong.FinalizationResult(); got.Unsettled || got.Err != nil {
		t.Fatalf("wrong owner finalization became unsettled: %+v", got)
	}

	for _, item := range []string{"item-1", "item-2"} {
		observer := factory().(*RealtimeTurnObserver)
		if err := observer.AdmitBoundedTurn(runtimesession.TurnAdmission{Models: models, WorkID: "work-" + item, InputItemID: item, Transcription: true}); err != nil {
			t.Fatal(err)
		}
		usage := &types.UsageEvent{Source: types.UsageSourceInputAudioTranscription, ItemID: item, BillingBasis: types.UsageBillingBasisDuration}
		usage.SetProviderIndependentUsageUnit(config.UsageExtraInputAudioTranscription, 9)
		usage.MarkProviderOperationUnits(1)
		if err := observer.ObserveTurnUsage(usage); err != nil {
			t.Fatal(err)
		}
		observer.FinalizeTurn(runtimesession.TurnFinalizePayload{TerminationReason: "transcription.completed", Usage: usage})
		observer.FinalizeTurn(runtimesession.TurnFinalizePayload{TerminationReason: "connection_closed", Usage: usage})
	}

	var user model.User
	var token model.Token
	if err := model.DB.First(&user, 1).Error; err != nil {
		t.Fatal(err)
	}
	if err := model.DB.First(&token, 1).Error; err != nil {
		t.Fatal(err)
	}
	if user.Quota != 1000 || token.RemainQuota != 1000 || user.UsedQuota != 2000 || token.UsedQuota != 2000 {
		t.Fatalf("two independent Realtime owners did not each settle once: user=%+v token=%+v", user, token)
	}
	var logs []model.Log
	if err := model.DB.Where("type = ?", model.LogTypeConsume).Find(&logs).Error; err != nil {
		t.Fatal(err)
	}
	if len(logs) != 2 {
		t.Fatalf("wrong attribution or repeated finalization wrote unexpected logs: %+v", logs)
	}
}

func issue054Price(priceType string, extraRatio *float64) *model.Price {
	price := &model.Price{Model: "i054-price", Type: priceType, Input: 1, Output: 1}
	if priceType == model.TimesPriceType {
		price.Output = 0
	}
	if extraRatio != nil {
		extra := datatypes.NewJSONType(map[string]float64{config.UsageExtraInputAudioTranscription: *extraRatio})
		price.ExtraRatios = &extra
	}
	return price
}

func issue054RateRules() *datatypes.JSONType[model.PriceRateRules] {
	rules := datatypes.NewJSONType(model.PriceRateRules{Version: 2, ServiceTier: []model.PriceRateRule{{ID: "flex", When: model.PriceRuleCondition{ServiceTier: []string{"flex"}}, Multipliers: model.PriceRateMultiplier{Input: utils.GetPointer(float64(2)), Output: utils.GetPointer(float64(3))}}}, LongContext: []model.PriceRateRule{{ID: "long_context", When: model.PriceRuleCondition{InputTokens: &model.PriceTokenRange{GT: utils.GetPointer(1)}}, Multipliers: model.PriceRateMultiplier{Input: utils.GetPointer(float64(4)), Output: utils.GetPointer(float64(5))}}}})
	return &rules
}

type issue054HTTPFactory struct {
	name        string
	channel     int
	newProvider func(*model.Channel, string) base.ProviderInterface
	azureAPI    string
}

func issue054HTTPFactories() []issue054HTTPFactory {
	return []issue054HTTPFactory{
		{
			name:    "openai",
			channel: config.ChannelTypeOpenAI,
			newProvider: func(channel *model.Channel, _ string) base.ProviderInterface {
				return openai.OpenAIProviderFactory{}.Create(channel)
			},
		},
		{
			name:     "azure",
			channel:  config.ChannelTypeAzure,
			azureAPI: "2024-10-01-preview",
			newProvider: func(channel *model.Channel, _ string) base.ProviderInterface {
				return azure.AzureProviderFactory{}.Create(channel)
			},
		},
		{
			name:    "azure-v1",
			channel: config.ChannelTypeAzureV1,
			newProvider: func(channel *model.Channel, _ string) base.ProviderInterface {
				return azure_v1.AzureV1ProviderFactory{}.Create(channel)
			},
		},
		{
			name:    "custom-openai-compatible",
			channel: config.ChannelTypeCustom,
			newProvider: func(channel *model.Channel, serverURL string) base.ProviderInterface {
				return openai.CreateOpenAIProvider(channel, serverURL)
			},
		},
		{
			name:    "siliconflow",
			channel: config.ChannelTypeSiliconflow,
			newProvider: func(channel *model.Channel, _ string) base.ProviderInterface {
				return siliconflow.SiliconflowProviderFactory{}.Create(channel)
			},
		},
	}
}

type issue054RealtimeFactory struct {
	name        string
	channel     int
	newProvider func(*model.Channel, string) base.ProviderInterface
	azureAPI    string
}

func issue054RealtimeFactories() []issue054RealtimeFactory {
	return []issue054RealtimeFactory{
		{
			name:    "openai",
			channel: config.ChannelTypeOpenAI,
			newProvider: func(channel *model.Channel, _ string) base.ProviderInterface {
				return openai.OpenAIProviderFactory{}.Create(channel)
			},
		},
		{
			name:     "azure",
			channel:  config.ChannelTypeAzure,
			azureAPI: "2024-10-01-preview",
			newProvider: func(channel *model.Channel, _ string) base.ProviderInterface {
				return azure.AzureProviderFactory{}.Create(channel)
			},
		},
		{
			name:    "azure-v1",
			channel: config.ChannelTypeAzureV1,
			newProvider: func(channel *model.Channel, _ string) base.ProviderInterface {
				return azure_v1.AzureV1ProviderFactory{}.Create(channel)
			},
		},
		{
			name:    "custom-openai-compatible",
			channel: config.ChannelTypeCustom,
			newProvider: func(channel *model.Channel, serverURL string) base.ProviderInterface {
				return openai.CreateOpenAIProvider(channel, serverURL)
			},
		},
	}
}

func issue054RealtimeUpstream(t *testing.T) (string, func()) {
	t.Helper()
	var commits atomic.Int32
	return wstest.Server(t, func(conn *wsconn.ManagedConn) {
		write := func(payload string) {
			if err := conn.WriteMessage(wsconn.TextMessage, []byte(payload)); err != nil {
				t.Errorf("write Realtime test event: %v", err)
			}
		}
		pump := wsconn.Pump{
			Conn: conn,
			Handle: func(_ context.Context, messageType wsconn.MessageType, payload []byte) {
				if messageType != wsconn.TextMessage {
					return
				}
				var envelope struct {
					Type string `json:"type"`
				}
				if json.Unmarshal(payload, &envelope) != nil {
					return
				}
				switch envelope.Type {
				case "session.update":
					write(`{"type":"session.updated"}`)
				case "input_audio_buffer.commit":
					index := commits.Add(1)
					itemID := fmt.Sprintf("i054-item-%d", index)
					write(fmt.Sprintf(`{"type":"input_audio_buffer.committed","item_id":%q}`, itemID))
					if index == 1 {
						write(`{"type":"conversation.item.input_audio_transcription.completed","item_id":"i054-wrong-item","content_index":0,"usage":{"type":"duration","seconds":9}}`)
					}
					completed := fmt.Sprintf(`{"type":"conversation.item.input_audio_transcription.completed","item_id":%q,"content_index":0,"usage":{"type":"duration","seconds":9}}`, itemID)
					write(completed)
					write(completed)
				}
			},
		}
		pump.Run(context.Background())
	})
}

func issue054SettleHTTPUsageSQL(t *testing.T, c *gin.Context, usage *types.Usage, want int64) {
	t.Helper()
	attempt, err := NewAttemptQuota(c, "i054-duration", 0, BillingAttemptSpec{LogProtocol: LogProtocolHTTP})
	if err != nil {
		t.Fatal(err)
	}
	if err := attempt.ApplyReserve(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := attempt.ClaimSubmission(); err != nil {
		t.Fatal(err)
	}
	result, err := attempt.CloseFromProviderResult(context.Background(), usage, false)
	if err != nil || result.Unsettled || !result.Confirmed || result.ChargedQuota != want {
		t.Fatalf("producer usage did not reach expected SQL settlement: result=%+v err=%v usage=%+v want=%d", result, err, usage, want)
	}
	second, err := attempt.CloseFromProviderResult(context.Background(), usage, false)
	if err != nil || second != result {
		t.Fatalf("repeated close changed producer settlement: first=%+v second=%+v err=%v", result, second, err)
	}
	var user model.User
	var token model.Token
	if err := model.DB.First(&user, 1).Error; err != nil {
		t.Fatal(err)
	}
	if err := model.DB.First(&token, 1).Error; err != nil {
		t.Fatal(err)
	}
	if user.Quota != int(1000-want) || token.RemainQuota != int(1000-want) || user.UsedQuota != int(want) || token.UsedQuota != int(want) {
		t.Fatalf("SQL user/token balances disagree with producer charge: user=%+v token=%+v want=%d", user, token, want)
	}
	var logs []model.Log
	if err := model.DB.Where("type = ?", model.LogTypeConsume).Find(&logs).Error; err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 || logs[0].Quota != int(want) || logs[0].PromptTokens != 0 || logs[0].CompletionTokens != 0 {
		t.Fatalf("producer settlement audit log mismatch or duplicate: %+v want=%d", logs, want)
	}
}

func float64PtrIssue054(value float64) *float64 {
	return &value
}

func issue054TranscriptionContext(t *testing.T, stream bool) *gin.Context {
	t.Helper()
	var raw bytes.Buffer
	writer := multipart.NewWriter(&raw)
	boundary := writer.Boundary()
	if err := writer.WriteField("model", "i054-duration"); err != nil {
		t.Fatal(err)
	}
	if stream {
		if err := writer.WriteField("stream", "true"); err != nil {
			t.Fatal(err)
		}
	}
	file, err := writer.CreateFormFile("file", "audio.wav")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("wave-bytes")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", bytes.NewReader(raw.Bytes()))
	ctx.Request.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	ctx.Request.ContentLength = int64(raw.Len())
	if _, err := common.CacheRequestBody(ctx); err != nil {
		t.Fatal(err)
	}
	ctx.Set("id", 1)
	ctx.Set("token_id", 1)
	ctx.Set("channel_id", 1)
	ctx.Set("token_name", "i054")
	ctx.Set("group_ratio", 1.0)
	groupctx.SetRoutingGroup(ctx, "paid", groupctx.RoutingGroupSourceUserGroup)
	return ctx
}
