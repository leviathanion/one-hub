package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"one-api/common/wsconn"
	runtimerealtime "one-api/runtime/realtime"
	runtimesession "one-api/runtime/session"
)

// This exercises the provider VAD path with create_response=false.  The
// provider still emits speech/commit notifications and a transcription
// terminal event, but it must not manufacture a response owner for this
// configuration.
func TestIssue044VADDisabledASRCompletionReleasesInputOverRealtime(t *testing.T) {
	server := newOpenAIRealtimeTestServer(t, func(conn *openAIRealtimeTestConn) {
		for {
			messageType, payload, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if messageType != wsconn.TextMessage {
				continue
			}
			var event struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(payload, &event); err != nil {
				t.Errorf("decode client event: %v", err)
				return
			}
			switch event.Type {
			case "session.update":
				if err := conn.WriteMessage(wsconn.TextMessage, []byte(`{"type":"session.updated","event_id":"server-settings"}`)); err != nil {
					return
				}
			case "input_audio_buffer.commit":
				// No response.created is sent: this is the pure ASR contract.
				providerEvents := []string{
					`{"type":"input_audio_buffer.speech_started","event_id":"server-speech-start","item_id":"asr-only-item"}`,
					`{"type":"input_audio_buffer.committed","event_id":"server-commit","item_id":"asr-only-item"}`,
					`{"type":"input_audio_buffer.speech_stopped","event_id":"server-speech-stop","item_id":"asr-only-item"}`,
					`{"type":"conversation.item.input_audio_transcription.completed","event_id":"server-asr-complete","item_id":"asr-only-item","content_index":0}`,
				}
				for _, providerEvent := range providerEvents {
					if err := conn.WriteMessage(wsconn.TextMessage, []byte(providerEvent)); err != nil {
						return
					}
				}
			}
		}
	})
	defer server.Close()

	provider := newOpenAIRealtimeTestProvider(server.URL)
	opened, apiErr := provider.OpenRealtimeSession("gpt-4o-realtime-preview")
	if apiErr != nil {
		t.Fatalf("open realtime session: %v", apiErr)
	}
	session, ok := opened.(*openAIRealtimeSession)
	if !ok {
		t.Fatalf("unexpected realtime session type %T", opened)
	}
	defer session.Abort("issue044_vad_disabled_cleanup")
	session.SetTurnObserverFactory(func() runtimesession.TurnObserver {
		return &realtimeInputTestObserver{}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	settings := []byte(`{"type":"session.update","event_id":"asr-only-settings","session":{"turn_detection":{"type":"server_vad","create_response":false},"input_audio_transcription":{"model":"whisper-1"}}}`)
	if err := session.SendClient(ctx, runtimerealtime.NewTextFrame(settings)); err != nil {
		t.Fatalf("send create_response=false settings: %v", err)
	}
	if err := issue044BoundaryReceiveType(t, ctx, session, "session.updated", false); err != nil {
		t.Fatal(err)
	}

	appendPayload := []byte(`{"type":"input_audio_buffer.append","event_id":"asr-only-append","audio":"AAAA"}`)
	if err := session.SendClient(ctx, runtimerealtime.NewTextFrame(appendPayload)); err != nil {
		t.Fatalf("send ASR append: %v", err)
	}
	session.mu.Lock()
	if len(session.inputWorks) != 1 || session.inputWorks[0] == nil {
		session.mu.Unlock()
		t.Fatalf("ASR append did not reserve one input owner: pending=%d", len(session.inputWorks))
	}
	work := session.inputWorks[0]
	automaticResponse := work.automaticResponse
	session.mu.Unlock()
	if automaticResponse {
		t.Fatal("create_response=false incorrectly created an automatic response owner")
	}

	commitPayload := []byte(`{"type":"input_audio_buffer.commit","event_id":"asr-only-commit"}`)
	if err := session.SendClient(ctx, runtimerealtime.NewTextFrame(commitPayload)); err != nil {
		t.Fatalf("send ASR commit: %v", err)
	}
	if err := issue044BoundaryReceiveType(t, ctx, session, "conversation.item.input_audio_transcription.completed", true); err != nil {
		t.Fatal(err)
	}

	session.mu.Lock()
	pending := len(session.inputWorks)
	automaticSourceSeen := work.automaticSourceSeen
	session.mu.Unlock()
	if !automaticSourceSeen {
		t.Fatalf("ASR speech events did not bind the input work: pending=%d work=%+v", pending, work)
	}
	if pending != 0 {
		t.Fatalf("pure ASR completion waited for a nonexistent response: pending=%d work=%+v", pending, work)
	}
}

// The explicit response is deliberately completed before the provider's VAD
// speech-stopped/committed notifications.  The later VAD response must still
// claim the work created by the real append, even when interrupt_response is
// disabled on the session.
func TestIssue044BusyTurnKeepsLaterVADOwnerAfterEarlierResponseCompletes(t *testing.T) {
	providerConn := make(chan *openAIRealtimeTestConn, 1)
	server := newOpenAIRealtimeTestServer(t, func(conn *openAIRealtimeTestConn) {
		providerConn <- conn
		for {
			messageType, payload, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if messageType != wsconn.TextMessage {
				continue
			}
			var event struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(payload, &event); err != nil {
				t.Errorf("decode client event: %v", err)
				return
			}
			switch event.Type {
			case "session.update":
				if err := conn.WriteMessage(wsconn.TextMessage, []byte(`{"type":"session.updated","event_id":"server-settings"}`)); err != nil {
					return
				}
			case "response.create":
				// Keep the first explicit response open until the test sends
				// response.done.  The later VAD response is provider initiated.
				if err := conn.WriteMessage(wsconn.TextMessage, []byte(`{"type":"response.created","event_id":"server-old-created","response":{"id":"old-response","status":"in_progress"}}`)); err != nil {
					return
				}
			}
		}
	})
	defer server.Close()

	provider := newOpenAIRealtimeTestProvider(server.URL)
	opened, apiErr := provider.OpenRealtimeSession("gpt-4o-realtime-preview")
	if apiErr != nil {
		t.Fatalf("open realtime session: %v", apiErr)
	}
	session, ok := opened.(*openAIRealtimeSession)
	if !ok {
		t.Fatalf("unexpected realtime session type %T", opened)
	}
	defer session.Abort("issue044_busy_vad_cleanup")

	var ownersMu sync.Mutex
	owners := make([]*realtimeInputTestObserver, 0, 2)
	session.SetTurnObserverFactory(func() runtimesession.TurnObserver {
		owner := &realtimeInputTestObserver{}
		ownersMu.Lock()
		owners = append(owners, owner)
		ownersMu.Unlock()
		return owner
	})
	upstream := <-providerConn

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	settings := []byte(`{"type":"session.update","event_id":"busy-vad-settings","session":{"turn_detection":{"type":"server_vad","create_response":true,"interrupt_response":false}}}`)
	if err := session.SendClient(ctx, runtimerealtime.NewTextFrame(settings)); err != nil {
		t.Fatalf("send busy VAD settings: %v", err)
	}
	if err := issue044BoundaryReceiveType(t, ctx, session, "session.updated", false); err != nil {
		t.Fatal(err)
	}

	oldResponse := []byte(`{"type":"response.create","event_id":"old-response-create","response":{}}`)
	if err := session.SendClient(ctx, runtimerealtime.NewTextFrame(oldResponse)); err != nil {
		t.Fatalf("send old response.create: %v", err)
	}
	if err := issue044BoundaryReceiveType(t, ctx, session, "response.created", false); err != nil {
		t.Fatal(err)
	}

	appendPayload := []byte(`{"type":"input_audio_buffer.append","event_id":"later-vad-append","audio":"AAAA"}`)
	if err := session.SendClient(ctx, runtimerealtime.NewTextFrame(appendPayload)); err != nil {
		t.Fatalf("send later VAD append: %v", err)
	}
	session.mu.Lock()
	if len(session.inputWorks) != 1 || session.inputWorks[0] == nil {
		session.mu.Unlock()
		t.Fatalf("later VAD append did not reserve one input owner: pending=%d", len(session.inputWorks))
	}
	work := session.inputWorks[0]
	automaticResponse := work.automaticResponse
	session.mu.Unlock()
	if !automaticResponse {
		t.Fatal("interrupt_response=false incorrectly disabled the later VAD response owner")
	}

	if err := upstream.WriteMessage(wsconn.TextMessage, []byte(`{"type":"response.done","event_id":"server-old-done","response":{"id":"old-response","status":"completed"}}`)); err != nil {
		t.Fatalf("complete old response: %v", err)
	}
	if err := issue044BoundaryReceiveType(t, ctx, session, "response.done", false); err != nil {
		t.Fatal(err)
	}
	session.mu.Lock()
	if len(session.inputWorks) != 1 || work.responseClaimed {
		session.mu.Unlock()
		t.Fatalf("old response completion changed the pending VAD owner: pending=%d claimed=%v", len(session.inputWorks), work.responseClaimed)
	}
	session.mu.Unlock()

	laterProviderEvents := []string{
		`{"type":"input_audio_buffer.speech_stopped","event_id":"server-later-speech-stop","item_id":"later-vad-item"}`,
		`{"type":"input_audio_buffer.committed","event_id":"server-later-commit","item_id":"later-vad-item"}`,
		`{"type":"response.created","event_id":"server-later-created","response":{"id":"later-response","status":"in_progress"}}`,
	}
	for _, providerEvent := range laterProviderEvents {
		if err := upstream.WriteMessage(wsconn.TextMessage, []byte(providerEvent)); err != nil {
			t.Fatalf("send later VAD provider event: %v", err)
		}
	}
	if err := issue044BoundaryReceiveType(t, ctx, session, "response.created", false); err != nil {
		t.Fatal(err)
	}

	ownersMu.Lock()
	if len(owners) != 2 {
		ownersMu.Unlock()
		t.Fatalf("expected old and later VAD owners, got %d", len(owners))
	}
	laterAdmission := owners[1].admission
	ownersMu.Unlock()
	if !laterAdmission.WorkAuthorized || laterAdmission.WorkID != work.id {
		t.Fatalf("later VAD response was not admitted to its original input owner: admission=%+v work=%+v", laterAdmission, work)
	}
	session.mu.Lock()
	pending := len(session.inputWorks)
	claimed := work.responseClaimed
	session.mu.Unlock()
	if !claimed || pending != 0 {
		t.Fatalf("later VAD response did not claim and release its original owner: claimed=%v pending=%d", claimed, pending)
	}

	if err := upstream.WriteMessage(wsconn.TextMessage, []byte(`{"type":"response.done","event_id":"server-later-done","response":{"id":"later-response","status":"completed"}}`)); err != nil {
		t.Fatalf("complete later VAD response: %v", err)
	}
	if err := issue044BoundaryReceiveType(t, ctx, session, "response.done", false); err != nil {
		t.Fatal(err)
	}
	ownersMu.Lock()
	defer ownersMu.Unlock()
	for i, owner := range owners {
		if owner.finalizeCount() != 1 {
			t.Fatalf("owner %d finalized %d times, want once", i, owner.finalizeCount())
		}
	}
}

// issue044BoundaryReceiveType consumes the real session receive path.  A
// response.created frame is optionally rejected so a pure ASR test cannot
// accidentally pass by waiting for a fabricated response.
func issue044BoundaryReceiveType(t *testing.T, ctx context.Context, session runtimerealtime.RealtimeSession, want string, rejectResponse bool) error {
	t.Helper()
	for {
		_, payload, _, _, err := openAITestRecv(ctx, session)
		if err != nil {
			return fmt.Errorf("receive %s: %w", want, err)
		}
		var event struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(payload, &event); err != nil {
			continue
		}
		if rejectResponse && event.Type == "response.created" {
			return fmt.Errorf("unexpected response.created while waiting for pure ASR completion")
		}
		if event.Type == want {
			return nil
		}
	}
}
