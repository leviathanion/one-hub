package relay_util

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"one-api/common/config"
	"one-api/common/requester"
	"one-api/model"
	"one-api/providers/base"
	runtimerealtime "one-api/runtime/realtime"
	runtimesession "one-api/runtime/session"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

func issue026WhisperDefaultPrice(t *testing.T) *model.Price {
	t.Helper()
	for _, price := range model.GetDefaultPrice() {
		if price.Model == "whisper-1" {
			return price
		}
	}
	t.Fatal("whisper-1 default price is missing")
	return nil
}

func issue026RealtimeBillingFixture(t *testing.T, quota int) *gin.Context {
	t.Helper()
	c := realtimeBillingFixture(t)
	if err := model.PricingInstance.AddPrice(issue026WhisperDefaultPrice(t)); err != nil {
		t.Fatal(err)
	}
	if err := model.DB.Model(&model.User{}).Where("id = ?", 1).Update("quota", quota).Error; err != nil {
		t.Fatal(err)
	}
	if err := model.DB.Model(&model.Token{}).Where("id = ?", 1).Update("remain_quota", quota).Error; err != nil {
		t.Fatal(err)
	}
	originalLog := config.LogConsumeEnabled
	originalReserve := config.PreConsumedQuota
	config.LogConsumeEnabled = true
	config.PreConsumedQuota = 50
	t.Cleanup(func() {
		config.LogConsumeEnabled = originalLog
		config.PreConsumedQuota = originalReserve
	})
	return c
}

func issue026SettleDurationHTTP(t *testing.T, c *gin.Context, usage *types.Usage, want int64, startingQuota int) {
	t.Helper()
	attempt, err := NewAttemptQuota(c, "whisper-1", 0, BillingAttemptSpec{LogProtocol: LogProtocolHTTP})
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
		t.Fatalf("duration HTTP settlement result=%+v error=%v want=%d", result, err, want)
	}
	second, err := attempt.CloseFromProviderResult(context.Background(), usage, false)
	if err != nil || second != result {
		t.Fatalf("duration HTTP repeated close changed result: first=%+v second=%+v error=%v", result, second, err)
	}

	var user model.User
	var token model.Token
	if err := model.DB.First(&user, 1).Error; err != nil {
		t.Fatal(err)
	}
	if err := model.DB.First(&token, 1).Error; err != nil {
		t.Fatal(err)
	}
	if user.Quota != startingQuota-int(want) || token.RemainQuota != startingQuota-int(want) || user.UsedQuota != int(want) || token.UsedQuota != int(want) {
		t.Fatalf("duration HTTP SQL balances mismatch: user=%+v token=%+v want=%d", user, token, want)
	}
	var logs []model.Log
	if err := model.DB.Where("type = ?", model.LogTypeConsume).Find(&logs).Error; err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 || logs[0].Quota != int(want) || logs[0].PromptTokens != 0 || logs[0].CompletionTokens != 0 {
		t.Fatalf("duration HTTP log mismatch or duplicate: %+v want=%d", logs, want)
	}
}

func TestI026WhisperDefaultHTTPDurationSettlesAtReferencePrice(t *testing.T) {
	for _, duration := range []float64{9, 60} {
		for _, factory := range issue054HTTPFactories() {
			t.Run(fmt.Sprintf("%s/%gs", factory.name, duration), func(t *testing.T) {
				const startingQuota = 500000
				c := issue026RealtimeBillingFixture(t, startingQuota)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_, _ = fmt.Fprintf(w, `{"text":"transcription-%gs","model":"whisper-1","usage":{"type":"duration","seconds":%g}}`, duration, duration)
				}))
				t.Cleanup(server.Close)
				previousClient := requester.HTTPClient
				requester.HTTPClient = server.Client()
				t.Cleanup(func() { requester.HTTPClient = previousClient })

				proxy, baseURL := "", server.URL
				channel := &model.Channel{Plugin: model.NewCustomEndpointPlugin(), Type: factory.channel, Key: "i026-key", Proxy: &proxy, BaseURL: &baseURL}
				if factory.azureAPI != "" {
					channel.Other = `{"api_version":"` + factory.azureAPI + `"}`
				}
				provider := factory.newProvider(channel, server.URL)
				provider.SetContext(issue054TranscriptionContext(t, false))
				provider.SetOriginalModel("whisper-1")
				provider.SetUsage(&types.Usage{})
				transcriber, ok := provider.(base.TranscriptionsInterface)
				if !ok {
					t.Fatalf("factory lost transcription interface: %T", provider)
				}
				response, apiErr := transcriber.CreateTranscriptions(&types.AudioRequest{Model: "whisper-1", ResponseFormat: "json"})
				if apiErr != nil || response == nil || response.Stream != nil {
					t.Fatalf("duration HTTP transcription failed: response=%+v error=%+v", response, apiErr)
				}
				if !strings.Contains(string(response.Body), fmt.Sprintf(`"text":"transcription-%gs"`, duration)) {
					t.Fatalf("client transcription body lost provider content: %s", response.Body)
				}
				usage := provider.GetUsage()
				if usage == nil || usage.ProviderOperationUnits == nil || *usage.ProviderOperationUnits != 1 ||
					!usage.ProviderIndependentUsageUnits[config.UsageExtraInputAudioTranscription] || usage.ExtraUsageUnits[config.UsageExtraInputAudioTranscription] != duration || usage.ResponseModel != "whisper-1" {
					t.Fatalf("duration HTTP evidence mismatch: %+v", usage)
				}
				issue026SettleDurationHTTP(t, c, usage, int64(duration*50), startingQuota)
			})
		}
	}
}

func TestI026WhisperDefaultRealtimeDurationSettlesAcrossFactories(t *testing.T) {
	for _, factory := range issue054RealtimeFactories() {
		t.Run(factory.name, func(t *testing.T) {
			const startingQuota = 500000
			c := issue026RealtimeBillingFixture(t, startingQuota)
			upstreamURL, cleanup := issue054RealtimeUpstream(t)
			t.Cleanup(cleanup)
			httpBaseURL := "http" + strings.TrimPrefix(upstreamURL, "ws")
			proxy := ""
			channel := &model.Channel{Plugin: model.NewCustomEndpointPlugin(), Type: factory.channel, Key: "i026-key", Proxy: &proxy}
			channel.BaseURL = &httpBaseURL
			if factory.azureAPI != "" {
				channel.Other = `{"api_version":"` + factory.azureAPI + `","self_hosted":true}`
			} else {
				channel.Other = `{"self_hosted":true}`
			}
			provider := factory.newProvider(channel, httpBaseURL)
			provider.SetContext(c)
			models := runtimesession.ModelBinding{RequestedModel: "whisper-1", ProviderModel: "whisper-1", BillingModel: "whisper-1"}
			realtimeProvider, ok := provider.(base.RealtimeSessionProviderWithOptions)
			if !ok {
				t.Fatalf("factory lost Realtime session interface: %T", provider)
			}
			session, apiErr := realtimeProvider.OpenRealtimeSessionWithOptions("whisper-1", runtimerealtime.RealtimeOpenOptions{Models: models, Context: context.Background()})
			if apiErr != nil || session == nil {
				t.Fatalf("Realtime session open failed: session=%T error=%+v", session, apiErr)
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

			if err := session.SendClient(ctx, runtimerealtime.NewTextFrame([]byte(`{"type":"session.update","session":{"turn_detection":null,"input_audio_transcription":{"model":"whisper-1"}}}`))); err != nil {
				t.Fatalf("send transcription configuration: %v", err)
			}
			completed := 0
			var clientPayloads []string
			waitForCompletions := func(want int) {
				t.Helper()
				for completed < want {
					select {
					case event := <-events:
						if event.Frame == nil {
							continue
						}
						payload := string(event.Frame.Payload())
						clientPayloads = append(clientPayloads, payload)
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
			if !containsI026DurationClientPayload(clientPayloads) {
				t.Fatalf("Realtime client did not receive duration=9 completion content: %+v", clientPayloads)
			}

			var user model.User
			var token model.Token
			if err := model.DB.First(&user, 1).Error; err != nil {
				t.Fatal(err)
			}
			if err := model.DB.First(&token, 1).Error; err != nil {
				t.Fatal(err)
			}
			if user.Quota != startingQuota-900 || token.RemainQuota != startingQuota-900 || user.UsedQuota != 900 || token.UsedQuota != 900 {
				t.Fatalf("Realtime duration SQL balances mismatch: user=%+v token=%+v", user, token)
			}
			var logs []model.Log
			if err := model.DB.Where("type = ?", model.LogTypeConsume).Find(&logs).Error; err != nil {
				t.Fatal(err)
			}
			if len(logs) != 2 || logs[0].Quota != 450 || logs[1].Quota != 450 || logs[0].PromptTokens != 0 || logs[1].CompletionTokens != 0 {
				t.Fatalf("Realtime duration logs mismatch or duplicate: %+v", logs)
			}
		})
	}
}

func containsI026DurationClientPayload(payloads []string) bool {
	for _, payload := range payloads {
		if strings.Contains(payload, `"type":"conversation.item.input_audio_transcription.completed"`) && strings.Contains(payload, `"seconds":9`) {
			return true
		}
	}
	return false
}
