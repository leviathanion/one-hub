package openai

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"one-api/common/config"
	"one-api/common/requester"
	"one-api/common/wsconn"
	"one-api/types"
)

func TestIssue054TranscriptionDurationRequiresFiniteNonNegativeSeconds(t *testing.T) {
	zero := 0.0
	valid := 9.0
	negative := -1.0
	nan := math.NaN()
	infinite := math.Inf(1)
	for _, test := range []struct {
		name       string
		seconds    *float64
		wantMarker bool
	}{
		{name: "missing"},
		{name: "zero", seconds: &zero, wantMarker: true},
		{name: "valid", seconds: &valid, wantMarker: true},
		{name: "negative", seconds: &negative},
		{name: "nan", seconds: &nan},
		{name: "infinite", seconds: &infinite},
	} {
		t.Run(test.name, func(t *testing.T) {
			target := &types.Usage{}
			applyOpenAITranscriptionUsage(target, &types.AudioUsage{Type: "duration", Seconds: test.seconds}, "whisper-actual")
			if test.wantMarker {
				if target.ProviderOperationUnits == nil || *target.ProviderOperationUnits != 1 {
					t.Fatalf("valid duration did not establish one operation: %+v", target)
				}
				if !target.ProviderIndependentUsageUnits[config.UsageExtraInputAudioTranscription] {
					t.Fatalf("valid duration did not retain its seconds evidence: %+v", target)
				}
				return
			}
			if target.ProviderOperationUnits != nil || target.ProviderIndependentUsageUnits[config.UsageExtraInputAudioTranscription] {
				t.Fatalf("invalid or missing duration established billing evidence: %+v", target)
			}
		})
	}
}

func TestIssue054OpenAITranscriptionJSONAndSSEPublishDurationUsage(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "sse"}[stream], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "event: transcript.text.done\ndata: {\"type\":\"transcript.text.done\",\"model\":\"whisper-duration\",\"usage\":{\"type\":\"duration\",\"seconds\":9}}\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"text":"ok","model":"whisper-duration","usage":{"type":"duration","seconds":9}}`)
			}))
			t.Cleanup(server.Close)
			previousClient := requester.HTTPClient
			requester.HTTPClient = server.Client()
			t.Cleanup(func() { requester.HTTPClient = previousClient })

			provider := issue005OpenAIProvider(t, server.URL, issue005OpenAITranscriptionContext(t, stream))
			response, apiErr := provider.CreateTranscriptions(&types.AudioRequest{Model: "whisper-1", ResponseFormat: "json", Stream: stream})
			if apiErr != nil || response == nil {
				t.Fatalf("duration transcription failed: response=%+v err=%+v", response, apiErr)
			}
			if stream {
				if response.Stream == nil || response.ObserveProviderEvent == nil {
					t.Fatalf("duration SSE lost its observable stream: %+v", response)
				}
				body, err := io.ReadAll(response.Stream.Body)
				_ = response.Stream.Body.Close()
				if err != nil {
					t.Fatal(err)
				}
				for _, event := range splitSSEDataForIssue054(string(body)) {
					response.ObserveProviderEvent(event)
				}
			}
			assertIssue054DurationUsage(t, provider.GetUsage(), !stream)
			if stream {
				return
			}
			if body, err := json.Marshal(response.Body); err != nil || len(body) == 0 {
				t.Fatalf("JSON transcription body disappeared: len=%d err=%v", len(body), err)
			}
		})
	}
}

func splitSSEDataForIssue054(body string) [][]byte {
	var payloads [][]byte
	for _, event := range strings.Split(body, "\n\n") {
		for _, line := range strings.Split(event, "\n") {
			if strings.HasPrefix(line, "data:") {
				payloads = append(payloads, []byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))))
			}
		}
	}
	return payloads
}

func assertIssue054DurationUsage(t *testing.T, usage *types.Usage, wantOperation bool) {
	t.Helper()
	if usage == nil {
		t.Fatal("duration completion lost its usage")
	}
	if wantOperation {
		if usage.ProviderOperationUnits == nil || *usage.ProviderOperationUnits != 1 {
			t.Fatalf("duration completion did not publish operation evidence: %+v", usage)
		}
	} else if usage.ProviderOperationUnits != nil {
		t.Fatalf("duration SSE unexpectedly published per-completion operation evidence: %+v", usage)
	}
	if !usage.ProviderIndependentUsageUnits[config.UsageExtraInputAudioTranscription] || usage.ExtraUsageUnits[config.UsageExtraInputAudioTranscription] != 9 {
		t.Fatalf("duration seconds evidence was not retained: %+v", usage)
	}
	if usage.ResponseModel != "whisper-duration" {
		t.Fatalf("actual duration model attribution was lost: %+v", usage)
	}
}

func TestIssue054RealtimeDurationRequiresPresentFiniteSeconds(t *testing.T) {
	zero := `{"event_id":"evt_zero","type":"conversation.item.input_audio_transcription.completed","item_id":"item_zero","usage":{"type":"duration","seconds":0}}`
	valid := `{"event_id":"evt_valid","type":"conversation.item.input_audio_transcription.completed","item_id":"item_valid","usage":{"type":"duration","seconds":9}}`
	missing := `{"event_id":"evt_missing","type":"conversation.item.input_audio_transcription.completed","item_id":"item_missing","usage":{"type":"duration"}}`
	negative := `{"event_id":"evt_negative","type":"conversation.item.input_audio_transcription.completed","item_id":"item_negative","usage":{"type":"duration","seconds":-1}}`
	tooLarge := `{"event_id":"evt_large","type":"conversation.item.input_audio_transcription.completed","item_id":"item_large","usage":{"type":"duration","seconds":1e309}}`
	for _, test := range []struct {
		name       string
		payload    string
		wantMarker bool
	}{
		{name: "zero", payload: zero, wantMarker: true},
		{name: "valid", payload: valid, wantMarker: true},
		{name: "missing", payload: missing},
		{name: "negative", payload: negative},
		{name: "nonfinite", payload: tooLarge},
	} {
		t.Run(test.name, func(t *testing.T) {
			usage := openAIRealtimeInputAudioTranscriptionUsage(types.EventTypeInputAudioTranscriptionCompleted, "", []byte(test.payload))
			if test.wantMarker {
				if usage == nil || usage.ProviderOperationUnits == nil || *usage.ProviderOperationUnits != 1 || !usage.ProviderIndependentUsageUnits[config.UsageExtraInputAudioTranscription] {
					t.Fatalf("valid Realtime duration was not authorized: %+v", usage)
				}
				return
			}
			if usage != nil && (usage.ProviderOperationUnits != nil || usage.ProviderIndependentUsageUnits[config.UsageExtraInputAudioTranscription]) {
				t.Fatalf("invalid or missing Realtime duration was authorized: %+v", usage)
			}
		})
	}
}

func TestIssue054RealtimeDurationCompletesItsInputOwnerOnce(t *testing.T) {
	s, _, owners := inputWorkFixture(t, false)
	admitTestInput(t, s, "duration-item")
	payload := []byte(`{"type":"conversation.item.input_audio_transcription.completed","item_id":"duration-item","content_index":0,"usage":{"type":"duration","seconds":9}}`)
	for range 2 {
		outbound, shouldClose := s.observeSupplierMessage(wsconn.TextMessage, payload)
		if shouldClose || outbound.err != nil {
			t.Fatalf("duration completion failed: outbound=%+v", outbound)
		}
	}
	if len(*owners) != 1 || (*owners)[0].observeCount() != 1 || (*owners)[0].finalizeCount() != 1 {
		t.Fatalf("duplicate duration completion reached the owner twice: %+v", *owners)
	}
	usage := (*owners)[0].lastPayload().Usage
	if usage == nil || usage.ProviderOperationUnits == nil || *usage.ProviderOperationUnits != 1 || usage.ExtraUsageUnits[config.UsageExtraInputAudioTranscription] != 9 {
		t.Fatalf("duration operation evidence did not survive the input owner: %+v", usage)
	}
}

func TestIssue054RealtimeWrongDurationItemDoesNotFinishAnotherOwner(t *testing.T) {
	s, _, owners := inputWorkFixture(t, false)
	admitTestInput(t, s, "owned-item")
	wrong := []byte(`{"type":"conversation.item.input_audio_transcription.completed","item_id":"other-item","content_index":0,"usage":{"type":"duration","seconds":9}}`)
	if outbound, shouldClose := s.observeSupplierMessage(wsconn.TextMessage, wrong); shouldClose || outbound.err != nil {
		t.Fatalf("wrong duration attribution caused session failure: outbound=%+v", outbound)
	}
	if len(*owners) != 1 || (*owners)[0].observeCount() != 0 || (*owners)[0].finalizeCount() != 0 {
		t.Fatalf("wrong duration item finished the owned input: %+v", *owners)
	}
	valid := []byte(`{"type":"conversation.item.input_audio_transcription.completed","item_id":"owned-item","content_index":0,"usage":{"type":"duration","seconds":9}}`)
	if outbound, shouldClose := s.observeSupplierMessage(wsconn.TextMessage, valid); shouldClose || outbound.err != nil {
		t.Fatalf("valid duration attribution failed: outbound=%+v", outbound)
	}
}
