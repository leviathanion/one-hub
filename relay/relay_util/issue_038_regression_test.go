package relay_util

import (
	"context"
	"encoding/json"
	"fmt"
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

// The provider generates its own event_id for session.updated; it does not
// echo the client request ID. The request ID remains available on rejection
// errors for precise failure correlation.
func issue038RealtimeUpstream(t *testing.T) (string, func()) {
	t.Helper()
	return wstest.Server(t, func(conn *wsconn.ManagedConn) {
		write := func(payload string) {
			if err := conn.WriteMessage(wsconn.TextMessage, []byte(payload)); err != nil {
				t.Errorf("write I038 Realtime event: %v", err)
			}
		}
		pump := wsconn.Pump{
			Conn: conn,
			Handle: func(_ context.Context, messageType wsconn.MessageType, payload []byte) {
				if messageType != wsconn.TextMessage {
					return
				}
				var event struct {
					Type    string          `json:"type"`
					EventID string          `json:"event_id"`
					Session json.RawMessage `json:"session"`
				}
				if json.Unmarshal(payload, &event) != nil {
					return
				}
				switch event.Type {
				case "session.update":
					var body struct {
						InputAudioTranscription *struct {
							Model string `json:"model"`
						} `json:"input_audio_transcription"`
					}
					_ = json.Unmarshal(event.Session, &body)
					modelName := ""
					if body.InputAudioTranscription != nil {
						modelName = strings.TrimSpace(body.InputAudioTranscription.Model)
					}
					switch modelName {
					case "i038-original":
						// This is a provider-generated acknowledgement ID, distinct
						// from the client request ID.
						write(`{"type":"session.updated","event_id":"srv-original","session":{"input_audio_transcription":{"model":"i038-original"}}}`)
					case "i038-next":
						// A rejected update may be delivered before an earlier
						// update's success acknowledgement. Duplicate errors must
						// remain harmless to the committed configuration.
						errorEvent := fmt.Sprintf(`{"type":"error","error":{"type":"invalid_request_error","code":"invalid_transcription_model","event_id":%q}}`, event.EventID)
						write(errorEvent)
						write(errorEvent)
						write(`{"type":"session.updated","event_id":"srv-instructions","session":{"instructions":"A"}}`)
					default:
						// The instructions-only update is intentionally held until
						// after the ASR rejection above, so its FIFO acknowledgement
						// cannot be mistaken for the rejected ASR update.
					}
				case "input_audio_buffer.commit":
					write(`{"type":"input_audio_buffer.committed","item_id":"i038-item"}`)
					write(`{"type":"conversation.item.input_audio_transcription.completed","item_id":"i038-item","content_index":0,"usage":{"type":"duration","seconds":1}}`)
					write(`{"type":"conversation.item.input_audio_transcription.completed","item_id":"i038-item","content_index":0,"usage":{"type":"duration","seconds":1}}`)
				}
			},
		}
		pump.Run(context.Background())
	})
}

func issue038Price(modelName string, ratio float64) *model.Price {
	ratioData := datatypes.NewJSONType(map[string]float64{
		config.UsageExtraInputAudioTranscription: ratio,
	})
	return &model.Price{
		Model:       modelName,
		Type:        model.TokensPriceType,
		Input:       1,
		Output:      0,
		ExtraRatios: &ratioData,
	}
}

func TestIssue038RealtimeRejectedASRConfigurationUsesLastConfirmedModelInSQL(t *testing.T) {
	for _, factory := range issue054RealtimeFactories() {
		t.Run(factory.name, func(t *testing.T) {
			c := realtimeBillingFixture(t)
			oldPricing, oldBatch, oldRedis, oldLog, oldReserve := model.PricingInstance, config.BatchUpdateEnabled, config.RedisEnabled, config.LogConsumeEnabled, config.PreConsumedQuota
			model.PricingInstance = &model.Pricing{Prices: map[string]*model.Price{
				"i038-original": issue038Price("i038-original", 3),
				"i038-next":     issue038Price("i038-next", 99),
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
			if err := model.DB.Model(&model.User{}).Where("id = ?", 1).Update("quota", 1000).Error; err != nil {
				t.Fatal(err)
			}
			if err := model.DB.Model(&model.Token{}).Where("id = ?", 1).Update("remain_quota", 1000).Error; err != nil {
				t.Fatal(err)
			}

			upstreamURL, cleanup := issue038RealtimeUpstream(t)
			t.Cleanup(cleanup)
			httpBaseURL := "http" + strings.TrimPrefix(upstreamURL, "ws")
			proxy := ""
			channel := &model.Channel{Type: factory.channel, Key: "i038-key", Proxy: &proxy}
			channel.BaseURL = &httpBaseURL
			if factory.azureAPI != "" {
				channel.Other = fmt.Sprintf(`{"api_version":%q,"self_hosted":true}`, factory.azureAPI)
			} else {
				channel.Other = `{"self_hosted":true}`
			}
			provider := factory.newProvider(channel, httpBaseURL)
			provider.SetContext(c)
			mainModels := runtimesession.ModelBinding{RequestedModel: "i038-session", ProviderModel: "i038-session", BillingModel: "i038-session"}
			realtimeProvider, ok := provider.(base.RealtimeSessionProviderWithOptions)
			if !ok {
				t.Fatalf("factory lost shared Realtime session interface: %T", provider)
			}
			session, apiErr := realtimeProvider.OpenRealtimeSessionWithOptions("i038-session", runtimerealtime.RealtimeOpenOptions{
				Models:  mainModels,
				Context: context.Background(),
			})
			if apiErr != nil || session == nil {
				t.Fatalf("Realtime session open failed: session=%T err=%+v", session, apiErr)
			}
			t.Cleanup(func() { session.Abort("I038 test cleanup") })
			session.SetTurnObserverFactory(NewRealtimeTurnObserverFactory(c, mainModels, nil))

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			for _, payload := range []string{
				`{"type":"session.update","event_id":"init","session":{"input_audio_transcription":{"model":"i038-original"}}}`,
				`{"type":"session.update","event_id":"instructions-a","session":{"instructions":"A"}}`,
				`{"type":"session.update","event_id":"asr-b","session":{"input_audio_transcription":{"model":"i038-next"}}}`,
			} {
				if err := session.SendClient(ctx, runtimerealtime.NewTextFrame([]byte(payload))); err != nil {
					t.Fatalf("send configuration %s: %v", payload, err)
				}
			}
			if err := session.SendClient(ctx, runtimerealtime.NewTextFrame([]byte(`{"type":"input_audio_buffer.commit"}`))); err != nil {
				t.Fatalf("send input commit: %v", err)
			}

			var completed, rejected int
			for completed == 0 || rejected < 2 {
				event, err := session.Recv(ctx)
				if err != nil {
					t.Fatalf("receive Realtime event: %v", err)
				}
				if event.Frame == nil {
					continue
				}
				var envelope struct {
					Type string `json:"type"`
				}
				if json.Unmarshal(event.Frame.Payload(), &envelope) != nil {
					continue
				}
				switch envelope.Type {
				case "error":
					rejected++
				case types.EventTypeInputAudioTranscriptionCompleted:
					completed++
				}
			}

			var user model.User
			var token model.Token
			if err := model.DB.First(&user, 1).Error; err != nil {
				t.Fatal(err)
			}
			if err := model.DB.First(&token, 1).Error; err != nil {
				t.Fatal(err)
			}
			if user.Quota != 997 || token.RemainQuota != 997 || user.UsedQuota != 3 || token.UsedQuota != 3 {
				t.Fatalf("rejected ASR model changed SQL charge: user=%+v token=%+v", user, token)
			}
			var logs []model.Log
			if err := model.DB.Where("type = ?", model.LogTypeConsume).Find(&logs).Error; err != nil {
				t.Fatal(err)
			}
			if len(logs) != 1 || logs[0].Quota != 3 || logs[0].ModelName != "i038-original" {
				t.Fatalf("rejected ASR model wrote wrong consume log: %+v", logs)
			}
		})
	}
}
