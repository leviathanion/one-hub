package relay_util

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/datatypes"
	"one-api/common/config"
	"one-api/common/wsconn"
	"one-api/common/wsconn/wstest"
	"one-api/model"
	"one-api/providers/azure"
	"one-api/providers/azure_v1"
	"one-api/providers/base"
	"one-api/providers/openai"
	runtimerealtime "one-api/runtime/realtime"
	runtimesession "one-api/runtime/session"
)

type issue044BillingFactory struct {
	name        string
	channelType int
	new         func(*model.Channel) base.ProviderInterface
}

func issue044BillingFactories() []issue044BillingFactory {
	return []issue044BillingFactory{
		{
			name:        "openai",
			channelType: config.ChannelTypeOpenAI,
			new: func(channel *model.Channel) base.ProviderInterface {
				return openai.OpenAIProviderFactory{}.Create(channel)
			},
		},
		{
			name:        "azure",
			channelType: config.ChannelTypeAzure,
			new: func(channel *model.Channel) base.ProviderInterface {
				return azure.AzureProviderFactory{}.Create(channel)
			},
		},
		{
			name:        "azure-v1",
			channelType: config.ChannelTypeAzureV1,
			new: func(channel *model.Channel) base.ProviderInterface {
				return azure_v1.AzureV1ProviderFactory{}.Create(channel)
			},
		},
		{
			name:        "custom-openai-compatible",
			channelType: config.ChannelTypeCustom,
			new: func(channel *model.Channel) base.ProviderInterface {
				return openai.OpenAIProviderFactory{}.Create(channel)
			},
		},
	}
}

type issue044BillingCounts struct {
	appends   atomic.Int32
	commits   atomic.Int32
	responses atomic.Int32
}

func issue044BillingUpstream(t *testing.T, counts *issue044BillingCounts, includeLateASR bool) (string, func()) {
	t.Helper()
	return wstest.Server(t, func(conn *wsconn.ManagedConn) {
		var itemSeq, responseSeq int
		lastItemID := ""
		write := func(payload string) bool {
			return conn.WriteMessage(wsconn.TextMessage, []byte(payload)) == nil
		}
		wsconn.Pump{
			Conn: conn,
			Handle: func(_ context.Context, messageType wsconn.MessageType, payload []byte) {
				if messageType != wsconn.TextMessage {
					return
				}
				var event struct {
					Type    string `json:"type"`
					EventID string `json:"event_id"`
				}
				if json.Unmarshal(payload, &event) != nil {
					return
				}
				switch event.Type {
				case "session.update":
					write(fmt.Sprintf(`{"type":"session.updated","event_id":"ack-%s"}`, event.EventID))
				case "input_audio_buffer.append":
					counts.appends.Add(1)
				case "input_audio_buffer.commit":
					counts.commits.Add(1)
					itemSeq++
					lastItemID = fmt.Sprintf("billing-item-%d", itemSeq)
					write(fmt.Sprintf(`{"type":"input_audio_buffer.committed","event_id":"commit-ack-%d","item_id":%q}`, itemSeq, lastItemID))
				case "response.create":
					counts.responses.Add(1)
					responseSeq++
					responseID := fmt.Sprintf("billing-response-%d", responseSeq)
					write(fmt.Sprintf(`{"type":"response.created","event_id":"response-created-%d","response":{"id":%q,"status":"in_progress"}}`, responseSeq, responseID))
					write(fmt.Sprintf(`{"type":"response.done","event_id":"response-done-%d","response":{"id":%q,"status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`, responseSeq, responseID))
					if includeLateASR && lastItemID != "" {
						write(fmt.Sprintf(`{"type":"conversation.item.input_audio_transcription.completed","event_id":"asr-done-%d","item_id":%q,"content_index":0,"usage":{"type":"tokens","input_tokens":1,"output_tokens":1,"total_tokens":2,"input_token_details":{"audio_tokens":1,"text_tokens":0},"output_token_details":{"text_tokens":1}}}`, responseSeq, lastItemID))
					}
				}
			},
		}.Run(context.Background())
	})
}

func issue044BillingReceiveRound(t *testing.T, ctx context.Context, session runtimerealtime.RealtimeSession, wantASR bool) {
	t.Helper()
	responseDone := false
	asrDone := false
	for {
		event, err := session.Recv(ctx)
		if err != nil {
			t.Fatalf("receive realtime response: %v", err)
		}
		if event.Err != nil {
			t.Fatalf("realtime provider error: %v", event.Err)
		}
		if event.Frame == nil || event.Frame.Kind() != runtimerealtime.FrameKindText {
			continue
		}
		var envelope struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(event.Frame.Payload(), &envelope) != nil {
			continue
		}
		if envelope.Type == "response.done" {
			responseDone = true
		}
		if envelope.Type == "conversation.item.input_audio_transcription.completed" {
			asrDone = true
		}
		if responseDone && (!wantASR || asrDone) {
			return
		}
	}
}

func TestIssue044RealtimeManualVADResponseSettlesOneOwnerPerRound(t *testing.T) {
	const rounds = 40
	const modelName = "i044-realtime"
	const chargePerRound = 2

	for _, factory := range issue044BillingFactories() {
		t.Run(factory.name, func(t *testing.T) {
			c := realtimeBillingFixture(t)
			oldPricing := model.PricingInstance
			oldBatch := config.BatchUpdateEnabled
			oldRedis := config.RedisEnabled
			oldLog := config.LogConsumeEnabled
			oldReserve := config.PreConsumedQuota
			audioRatio := datatypes.NewJSONType(map[string]float64{config.UsageExtraInputAudio: 4})
			model.PricingInstance = &model.Pricing{Prices: map[string]*model.Price{
				modelName: {Model: modelName, Type: model.TokensPriceType, Input: 1, Output: 1, ExtraRatios: &audioRatio},
			}}
			config.BatchUpdateEnabled = false
			config.RedisEnabled = false
			config.LogConsumeEnabled = true
			config.PreConsumedQuota = 50
			if err := model.DB.Model(&model.User{}).Where("id = ?", 1).Update("quota", 100000).Error; err != nil {
				t.Fatal(err)
			}
			if err := model.DB.Model(&model.Token{}).Where("id = ?", 1).Update("remain_quota", 100000).Error; err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				model.PricingInstance = oldPricing
				config.BatchUpdateEnabled = oldBatch
				config.RedisEnabled = oldRedis
				config.LogConsumeEnabled = oldLog
				config.PreConsumedQuota = oldReserve
			})

			counts := &issue044BillingCounts{}
			upstreamURL, cleanup := issue044BillingUpstream(t, counts, true)
			t.Cleanup(cleanup)
			httpBaseURL := "http" + strings.TrimPrefix(upstreamURL, "ws")
			proxy := ""
			other := `{"self_hosted":true}`
			if factory.channelType == config.ChannelTypeAzure {
				other = `{"self_hosted":true,"api_version":"2025-04-01-preview"}`
			}
			channel := &model.Channel{Type: factory.channelType, Key: "i044-key", Proxy: &proxy, BaseURL: &httpBaseURL, Other: other}
			provider := factory.new(channel)
			provider.SetContext(c)
			models := runtimesession.ModelBinding{RequestedModel: modelName, ProviderModel: modelName, BillingModel: modelName}
			realtimeProvider, ok := provider.(base.RealtimeSessionProviderWithOptions)
			if !ok {
				t.Fatalf("factory lost realtime interface: %T", provider)
			}
			session, apiErr := realtimeProvider.OpenRealtimeSessionWithOptions(modelName, runtimerealtime.RealtimeOpenOptions{Models: models, Context: context.Background()})
			if apiErr != nil || session == nil {
				t.Fatalf("open realtime session: session=%T err=%+v", session, apiErr)
			}
			t.Cleanup(func() { session.Abort("issue044_cleanup") })
			session.SetTurnObserverFactory(NewRealtimeTurnObserverFactory(c, models, nil))

			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			settings := []byte(fmt.Sprintf(`{"type":"session.update","event_id":"issue044-settings","session":{"turn_detection":{"type":"server_vad","create_response":true},"input_audio_transcription":{"model":%q}}}`, modelName))
			if err := session.SendClient(ctx, runtimerealtime.NewTextFrame(settings)); err != nil {
				t.Fatalf("send VAD settings: %v", err)
			}
			for round := 1; round <= rounds; round++ {
				if err := session.SendClient(ctx, runtimerealtime.NewTextFrame([]byte(fmt.Sprintf(`{"type":"input_audio_buffer.append","event_id":"append-%d","audio":"AAAA"}`, round)))); err != nil {
					t.Fatalf("round %d append: %v", round, err)
				}
				if err := session.SendClient(ctx, runtimerealtime.NewTextFrame([]byte(fmt.Sprintf(`{"type":"input_audio_buffer.commit","event_id":"commit-%d"}`, round)))); err != nil {
					t.Fatalf("round %d commit: %v", round, err)
				}
				if err := session.SendClient(ctx, runtimerealtime.NewTextFrame([]byte(fmt.Sprintf(`{"type":"response.create","event_id":"response-create-%d","response":{}}`, round)))); err != nil {
					t.Fatalf("round %d response.create: %v", round, err)
				}
				issue044BillingReceiveRound(t, ctx, session, true)
			}

			if counts.appends.Load() != rounds || counts.commits.Load() != rounds || counts.responses.Load() != rounds {
				t.Fatalf("upstream operation counts: appends=%d commits=%d responses=%d", counts.appends.Load(), counts.commits.Load(), counts.responses.Load())
			}
			var user model.User
			var token model.Token
			if err := model.DB.First(&user, 1).Error; err != nil {
				t.Fatal(err)
			}
			if err := model.DB.First(&token, 1).Error; err != nil {
				t.Fatal(err)
			}
			wantCharge := rounds * (chargePerRound + 5)
			if user.UsedQuota != wantCharge || token.UsedQuota != wantCharge || user.Quota != 100000-wantCharge || token.RemainQuota != 100000-wantCharge {
				t.Fatalf("SQL owner settlement mismatch: user=%+v token=%+v wantCharge=%d", user, token, wantCharge)
			}
			var logs []model.Log
			if err := model.DB.Where("type = ?", model.LogTypeConsume).Find(&logs).Error; err != nil {
				t.Fatal(err)
			}
			if len(logs) != rounds*2 {
				t.Fatalf("each response and late ASR owner must settle exactly once: log count=%d want=%d", len(logs), rounds*2)
			}
			var responseCharges, asrCharges int
			for _, entry := range logs {
				switch entry.Quota {
				case chargePerRound:
					responseCharges++
				case 5:
					asrCharges++
				default:
					t.Fatalf("unexpected owner charge=%d: %+v", entry.Quota, entry)
				}
			}
			if responseCharges != rounds || asrCharges != rounds {
				t.Fatalf("response/late ASR owner charges: response=%d asr=%d want each=%d", responseCharges, asrCharges, rounds)
			}
		})
	}
}
