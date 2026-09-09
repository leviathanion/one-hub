package openai_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

type issue044Factory struct {
	name        string
	channelType int
	new         func(*model.Channel) base.ProviderInterface
}

func issue044Factories() []issue044Factory {
	return []issue044Factory{
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

func issue044Channel(factory issue044Factory, endpoint string) *model.Channel {
	proxy := ""
	other := `{"self_hosted":true}`
	if factory.channelType == config.ChannelTypeAzure {
		other = `{"self_hosted":true,"api_version":"2025-04-01-preview"}`
	}
	return &model.Channel{
		Type:    factory.channelType,
		Key:     "issue-044-key",
		Proxy:   &proxy,
		BaseURL: &endpoint,
		Other:   other,
	}
}

type issue044FactoryCounts struct {
	appends   atomic.Int32
	commits   atomic.Int32
	responses atomic.Int32
}

// issue044Server is a local provider protocol peer. It emits the provider's
// commit acknowledgement before the explicit response lifecycle, preserving
// the ordering that previously left the input owner waiting forever.
func issue044Server(t *testing.T, counts *issue044FactoryCounts) (string, func()) {
	t.Helper()
	return wstest.Server(t, func(conn *wsconn.ManagedConn) {
		frames := make(chan []byte, 64)
		var closeOnce sync.Once
		closeFrames := func() { closeOnce.Do(func() { close(frames) }) }
		go wsconn.Pump{
			Conn: conn,
			Handle: func(_ context.Context, _ wsconn.MessageType, payload []byte) {
				select {
				case frames <- append([]byte(nil), payload...):
				case <-conn.Done():
				}
			},
			OnClose: func(wsconn.CloseInfo) { closeFrames() },
		}.Run(context.Background())

		var itemSeq, responseSeq int
		for payload := range frames {
			var event struct {
				Type    string `json:"type"`
				EventID string `json:"event_id"`
			}
			if err := json.Unmarshal(payload, &event); err != nil {
				t.Errorf("decode realtime client event: %v", err)
				return
			}
			switch event.Type {
			case "session.update":
				ack := fmt.Sprintf(`{"type":"session.updated","event_id":"ack-%s","session":{"turn_detection":{"type":"server_vad","create_response":true}}}`, event.EventID)
				if err := conn.WriteMessage(wsconn.TextMessage, []byte(ack)); err != nil {
					return
				}
			case "input_audio_buffer.append":
				counts.appends.Add(1)
			case "input_audio_buffer.commit":
				counts.commits.Add(1)
				itemSeq++
				committed := fmt.Sprintf(`{"type":"input_audio_buffer.committed","event_id":"server-commit-%d","item_id":"manual-item-%d"}`, itemSeq, itemSeq)
				if err := conn.WriteMessage(wsconn.TextMessage, []byte(committed)); err != nil {
					return
				}
			case "response.create":
				counts.responses.Add(1)
				responseSeq++
				responseID := fmt.Sprintf("response-%d", responseSeq)
				created := fmt.Sprintf(`{"type":"response.created","event_id":"provider-created-%d","response":{"id":%q,"status":"in_progress"}}`, responseSeq, responseID)
				done := fmt.Sprintf(`{"type":"response.done","event_id":"provider-done-%d","response":{"id":%q,"status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`, responseSeq, responseID)
				if err := conn.WriteMessage(wsconn.TextMessage, []byte(created)); err != nil {
					return
				}
				if err := conn.WriteMessage(wsconn.TextMessage, []byte(done)); err != nil {
					return
				}
			}
		}
	})
}

func issue044ReceiveDone(t *testing.T, ctx context.Context, session runtimerealtime.RealtimeSession) {
	t.Helper()
	for {
		event, err := session.Recv(ctx)
		if err != nil {
			t.Fatalf("receive response lifecycle: %v", err)
		}
		if event.Err != nil {
			t.Fatalf("provider returned realtime error: %v", event.Err)
		}
		if event.Frame == nil || event.Frame.Kind() != runtimerealtime.FrameKindText {
			continue
		}
		var envelope struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(event.Frame.Payload(), &envelope) == nil && envelope.Type == "response.done" {
			return
		}
	}
}

func TestIssue044ManualVADCommitReleasesInputCapacityAcrossFactories(t *testing.T) {
	for _, factory := range issue044Factories() {
		t.Run(factory.name, func(t *testing.T) {
			counts := &issue044FactoryCounts{}
			endpoint, cleanup := issue044Server(t, counts)
			defer cleanup()

			provider, ok := factory.new(issue044Channel(factory, "http"+endpoint[len("ws"):])).(base.RealtimeSessionProvider)
			if !ok {
				t.Fatalf("factory does not expose realtime session: %T", factory.new(issue044Channel(factory, "http"+endpoint[len("ws"):])))
			}
			session, apiErr := provider.OpenRealtimeSession("gpt-4o-realtime-preview")
			if apiErr != nil {
				t.Fatalf("open realtime session: %v", apiErr)
			}
			defer session.Abort("issue044_cleanup")

			var observerMu sync.Mutex
			finalized := make([]int, 0, 40)
			session.SetTurnObserverFactory(func() runtimesession.TurnObserver {
				var finalizedOnce sync.Once
				return runtimesession.TurnObserverFunc(func(_ runtimesession.TurnFinalizePayload) {
					finalizedOnce.Do(func() {
						observerMu.Lock()
						finalized = append(finalized, 1)
						observerMu.Unlock()
					})
				})
			})

			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			settings := []byte(`{"type":"session.update","event_id":"issue044-settings","session":{"turn_detection":{"type":"server_vad","create_response":true}}}`)
			if err := session.SendClient(ctx, runtimerealtime.NewTextFrame(settings)); err != nil {
				t.Fatalf("send VAD settings: %v", err)
			}

			for i := 1; i <= 40; i++ {
				appendPayload := []byte(fmt.Sprintf(`{"type":"input_audio_buffer.append","event_id":"append-%d","audio":"AAAA"}`, i))
				commitPayload := []byte(fmt.Sprintf(`{"type":"input_audio_buffer.commit","event_id":"commit-%d"}`, i))
				responsePayload := []byte(fmt.Sprintf(`{"type":"response.create","event_id":"response-create-%d","response":{}}`, i))
				if err := session.SendClient(ctx, runtimerealtime.NewTextFrame(appendPayload)); err != nil {
					t.Fatalf("round %d append: %v", i, err)
				}
				if err := session.SendClient(ctx, runtimerealtime.NewTextFrame(commitPayload)); err != nil {
					t.Fatalf("round %d commit: %v", i, err)
				}
				if err := session.SendClient(ctx, runtimerealtime.NewTextFrame(responsePayload)); err != nil {
					t.Fatalf("round %d response.create: %v", i, err)
				}
				issue044ReceiveDone(t, ctx, session)
			}
			if got := counts.appends.Load(); got != 40 {
				t.Fatalf("upstream append count=%d, want 40", got)
			}
			if got := counts.commits.Load(); got != 40 {
				t.Fatalf("upstream commit count=%d, want 40", got)
			}
			if got := counts.responses.Load(); got != 40 {
				t.Fatalf("upstream response count=%d, want 40", got)
			}
			observerMu.Lock()
			defer observerMu.Unlock()
			if len(finalized) != 40 {
				t.Fatalf("response owners finalized=%d, want 40", len(finalized))
			}
		})
	}
}
