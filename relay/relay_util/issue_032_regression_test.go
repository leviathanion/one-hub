package relay_util

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"one-api/common/config"
	"one-api/common/wsconn"
	"one-api/common/wsconn/wstest"
	"one-api/model"
	"one-api/providers/base"
	runtimerealtime "one-api/runtime/realtime"
	runtimesession "one-api/runtime/session"
	"one-api/types"

	"gorm.io/datatypes"
)

func TestIssue032RealtimeTokenFactoriesCarryCompleteUsageToOwnSQL(t *testing.T) {
	for _, factory := range issue054RealtimeFactories() {
		t.Run(factory.name, func(t *testing.T) {
			c := realtimeBillingFixture(t)
			oldPricing, oldBatch, oldRedis, oldLog, oldReserve := model.PricingInstance, config.BatchUpdateEnabled, config.RedisEnabled, config.LogConsumeEnabled, config.PreConsumedQuota
			audioRatio := datatypes.NewJSONType(map[string]float64{config.UsageExtraInputAudio: 4})
			model.PricingInstance = &model.Pricing{Prices: map[string]*model.Price{
				"i032-realtime": {Model: "i032-realtime", Type: model.TokensPriceType, Input: 2, Output: 3, ExtraRatios: &audioRatio},
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

			upstreamURL, cleanup := issue032RealtimeTokenUpstream(t)
			t.Cleanup(cleanup)
			httpBaseURL := "http" + strings.TrimPrefix(upstreamURL, "ws")
			proxy := ""
			channel := &model.Channel{Type: factory.channel, Key: "i032-key", Proxy: &proxy, BaseURL: &httpBaseURL}
			if factory.azureAPI != "" {
				channel.Other = `{"api_version":"` + factory.azureAPI + `","self_hosted":true}`
			} else {
				channel.Other = `{"self_hosted":true}`
			}
			provider := factory.newProvider(channel, httpBaseURL)
			provider.SetContext(c)
			models := runtimesession.ModelBinding{RequestedModel: "i032-realtime", ProviderModel: "i032-realtime", BillingModel: "i032-realtime"}
			realtimeProvider, ok := provider.(base.RealtimeSessionProviderWithOptions)
			if !ok {
				t.Fatalf("factory lost shared Realtime session interface: %T", provider)
			}
			session, apiErr := realtimeProvider.OpenRealtimeSessionWithOptions("i032-realtime", runtimerealtime.RealtimeOpenOptions{Models: models, Context: context.Background()})
			if apiErr != nil || session == nil {
				t.Fatalf("Realtime session open failed: session=%T err=%+v", session, apiErr)
			}
			t.Cleanup(func() { session.Abort("test_cleanup") })
			session.SetTurnObserverFactory(NewRealtimeTurnObserverFactory(c, models, nil))

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			events := make(chan runtimerealtime.RecvEvent, 8)
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
				select {
				case <-receiverDone:
				case <-time.After(time.Second):
					t.Error("timed out waiting for Realtime receiver to stop")
				}
			}()

			if err := session.SendClient(ctx, runtimerealtime.NewTextFrame([]byte(`{"type":"session.update","session":{"turn_detection":null,"input_audio_transcription":{"model":"i032-realtime"}}}`))); err != nil {
				t.Fatalf("send transcription configuration: %v", err)
			}
			if err := session.SendClient(ctx, runtimerealtime.NewTextFrame([]byte(`{"type":"input_audio_buffer.commit"}`))); err != nil {
				t.Fatalf("send audio commit: %v", err)
			}

			var completed []runtimerealtime.RecvEvent
			for len(completed) < 2 {
				select {
				case event := <-events:
					if event.Frame == nil {
						continue
					}
					var envelope struct {
						Type string `json:"type"`
					}
					if json.Unmarshal(event.Frame.Payload(), &envelope) == nil && envelope.Type == types.EventTypeInputAudioTranscriptionCompleted {
						completed = append(completed, event)
					}
				case err := <-errorsCh:
					t.Fatalf("Realtime receive failed before duplicate token completions: %v", err)
				case <-ctx.Done():
					t.Fatalf("timed out waiting for duplicate token completions: %v", ctx.Err())
				}
			}
			if completed[0].Usage == nil || !completed[0].Usage.ProviderTokenEvidence || completed[0].Usage.ProviderTokenConflict ||
				completed[0].Usage.InputTokens != 13 || completed[0].Usage.OutputTokens != 9 || completed[0].Usage.TotalTokens != 22 ||
				completed[0].Usage.InputTokenDetails.AudioTokens != 13 || completed[0].Usage.ResponseModel != "i032-realtime" {
				t.Fatalf("complete token evidence did not reach the client owner: %+v", completed[0].Usage)
			}

			var user model.User
			var token model.Token
			if err := model.DB.First(&user, 1).Error; err != nil {
				t.Fatal(err)
			}
			if err := model.DB.First(&token, 1).Error; err != nil {
				t.Fatal(err)
			}
			if user.Quota != 869 || token.RemainQuota != 869 || user.UsedQuota != 131 || token.UsedQuota != 131 {
				t.Fatalf("token ASR SQL settlement lost output/audio pricing: user=%+v token=%+v", user, token)
			}
			var logs []model.Log
			if err := model.DB.Where("type = ?", model.LogTypeConsume).Find(&logs).Error; err != nil {
				t.Fatal(err)
			}
			if len(logs) != 1 || logs[0].Quota != 131 || logs[0].PromptTokens != 13 || logs[0].CompletionTokens != 9 || logs[0].ModelName != "i032-realtime" {
				t.Fatalf("token ASR audit log mismatch or duplicate: %+v", logs)
			}
		})
	}
}

func TestIssue032OptionalPartitionsFollowEffectivePriceInSQL(t *testing.T) {
	for _, test := range []struct {
		name            string
		audioRatio      *float64
		inputTextTokens *int
		outputTextRatio *float64
		wantCharge      int64
	}{
		{name: "base price", wantCharge: 53},
		{name: "audio difference price with both partitions missing", audioRatio: float64PtrIssue032(4), wantCharge: 0},
		{name: "audio difference price with known zero text remainder", audioRatio: float64PtrIssue032(4), inputTextTokens: intPtrIssue032(0), wantCharge: 131},
		{name: "output detail price", outputTextRatio: float64PtrIssue032(2), wantCharge: 80},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := realtimeBillingFixture(t)
			oldPricing, oldBatch, oldRedis, oldLog, oldReserve := model.PricingInstance, config.BatchUpdateEnabled, config.RedisEnabled, config.LogConsumeEnabled, config.PreConsumedQuota
			price := &model.Price{Model: "i032-missing-audio", Type: model.TokensPriceType, Input: 2, Output: 3}
			extraValues := make(map[string]float64)
			if test.audioRatio != nil {
				extraValues[config.UsageExtraInputAudio] = *test.audioRatio
			}
			if test.outputTextRatio != nil {
				extraValues[config.UsageExtraOutputTextTokens] = *test.outputTextRatio
			}
			if len(extraValues) > 0 {
				extra := datatypes.NewJSONType(extraValues)
				price.ExtraRatios = &extra
			}
			model.PricingInstance = &model.Pricing{Prices: map[string]*model.Price{price.Model: price}}
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

			models := runtimesession.ModelBinding{RequestedModel: price.Model, ProviderModel: price.Model, BillingModel: price.Model}
			observer := &RealtimeTurnObserver{requestContext: c, models: models}
			if err := observer.AdmitBoundedTurn(runtimesession.TurnAdmission{Models: models, WorkID: "i032-missing-audio", Transcription: true, WorkAuthorized: true}); err != nil {
				t.Fatal(err)
			}
			usage := &types.UsageEvent{
				InputTokens:           13,
				OutputTokens:          9,
				TotalTokens:           22,
				ProviderTokenEvidence: true,
				ProviderTokenFields: map[string]bool{
					"prompt_tokens": true, "completion_tokens": true, "total_tokens": true,
				},
				Source:       types.UsageSourceInputAudioTranscription,
				BillingBasis: types.UsageBillingBasisTokens,
			}
			usage.RequireTokenExtraEvidence(config.UsageExtraInputAudio, config.UsageExtraInputTextTokens)
			usage.SetTokenExtraEvidenceGroups([]string{config.UsageExtraInputAudio, config.UsageExtraInputTextTokens})
			if test.inputTextTokens != nil {
				usage.InputTokenDetails.TextTokens = *test.inputTextTokens
				usage.ProviderTokenFields[config.UsageExtraInputTextTokens] = true
				usage.SetExtraTokens(config.UsageExtraInputTextTokens, *test.inputTextTokens)
			}
			if test.outputTextRatio != nil {
				usage.OutputTokenDetails.TextTokens = 9
				usage.ProviderTokenFields[config.UsageExtraOutputTextTokens] = true
				usage.SetExtraTokens(config.UsageExtraOutputTextTokens, 9)
			}
			if err := observer.ObserveTurnUsage(usage); err != nil {
				t.Fatal(err)
			}
			observer.FinalizeTurn(runtimesession.TurnFinalizePayload{TerminationReason: "transcription.completed", Usage: usage})

			var user model.User
			var token model.Token
			if err := model.DB.First(&user, 1).Error; err != nil {
				t.Fatal(err)
			}
			if err := model.DB.First(&token, 1).Error; err != nil {
				t.Fatal(err)
			}
			if user.Quota != int(1000-test.wantCharge) || token.RemainQuota != int(1000-test.wantCharge) || user.UsedQuota != int(test.wantCharge) || token.UsedQuota != int(test.wantCharge) {
				t.Fatalf("missing audio partition changed SQL settlement: user=%+v token=%+v want=%d", user, token, test.wantCharge)
			}
			var logs []model.Log
			if err := model.DB.Where("type = ?", model.LogTypeConsume).Find(&logs).Error; err != nil {
				t.Fatal(err)
			}
			if len(logs) != boolToIntIssue032(test.wantCharge > 0) {
				t.Fatalf("missing audio partition wrote unexpected consume logs: %+v", logs)
			}
			if test.wantCharge > 0 && logs[0].Quota != int(test.wantCharge) {
				t.Fatalf("base token charge log=%+v want=%d", logs[0], test.wantCharge)
			}
		})
	}
}

func float64PtrIssue032(value float64) *float64 {
	return &value
}

func intPtrIssue032(value int) *int {
	return &value
}

func boolToIntIssue032(value bool) int {
	if value {
		return 1
	}
	return 0
}

func issue032RealtimeTokenUpstream(t *testing.T) (string, func()) {
	t.Helper()
	return wstest.Server(t, func(conn *wsconn.ManagedConn) {
		write := func(payload string) {
			if err := conn.WriteMessage(wsconn.TextMessage, []byte(payload)); err != nil {
				t.Errorf("write Realtime token test event: %v", err)
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
					write(`{"type":"input_audio_buffer.committed","item_id":"i032-token-item"}`)
					completed := `{"type":"conversation.item.input_audio_transcription.completed","item_id":"i032-token-item","content_index":0,"usage":{"type":"tokens","input_tokens":13,"output_tokens":9,"total_tokens":22,"input_token_details":{"audio_tokens":13,"text_tokens":0},"output_token_details":{"text_tokens":9}}}`
					write(completed)
					write(completed)
				}
			},
		}
		pump.Run(context.Background())
	})
}
