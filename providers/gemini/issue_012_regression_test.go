package gemini

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"one-api/common/requester"
	"one-api/types"
)

func collectNativeGeminiStream(t *testing.T, requestBody, responseWire string) (string, error, *types.Usage) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil || string(body) != requestBody {
			t.Errorf("原生请求 wire 被改变: body=%s err=%v", body, err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, responseWire)
	}))
	t.Cleanup(server.Close)

	previousClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = previousClient })

	provider, request := newNativeGeminiProviderForTest(t, server, requestBody, true)
	stream, apiErr := provider.CreateGeminiChatStream(request)
	if apiErr != nil {
		t.Fatalf("open native Gemini stream: %+v", apiErr)
	}
	dataChan, errChan := stream.Recv()
	t.Cleanup(stream.Close)

	var data strings.Builder
	var streamErr error
	for dataChan != nil || errChan != nil {
		select {
		case chunk, ok := <-dataChan:
			if !ok {
				dataChan = nil
				continue
			}
			data.WriteString(chunk)
		case err, ok := <-errChan:
			if !ok {
				errChan = nil
				continue
			}
			if err != nil {
				streamErr = err
			}
		}
	}
	return data.String(), streamErr, provider.GetUsage()
}

func TestNativeGeminiBlockedPromptIsAValidStreamTerminal(t *testing.T) {
	for _, blockReason := range []string{"SAFETY", "BLOCKLIST"} {
		t.Run(blockReason, func(t *testing.T) {
			block := "data: {\"promptFeedback\":{\"blockReason\":\"" + blockReason + "\"}}\n\n"
			usage := "data: {\"usageMetadata\":{\"promptTokenCount\":2,\"candidatesTokenCount\":0,\"totalTokenCount\":2}}\n\n"
			extra := "data: {\"future\":{\"afterBlock\":true}}\n\n"
			wire := block + usage + extra
			got, streamErr, providerUsage := collectNativeGeminiStream(t, `{"contents":[]}`, wire)
			if got != wire {
				t.Fatalf("blocked Gemini response was changed: got %q, want %q", got, wire)
			}
			if !errors.Is(streamErr, io.EOF) || errors.Is(streamErr, requester.ErrStreamProtocolTerminalMissing) {
				t.Fatalf("blocked Gemini response was not accepted as a terminal: %v", streamErr)
			}
			if providerUsage.PromptTokens != 2 || providerUsage.TotalTokens != 2 || !providerUsage.ProviderReported {
				t.Fatalf("usage after blocked response was lost: %+v", providerUsage)
			}
		})
	}
}

func TestNativeGeminiStreamKeepsAllCandidatesAndFinalUsage(t *testing.T) {
	first := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"first\"}]},\"finishReason\":\"STOP\",\"index\":0}]}\n\n"
	second := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"second\"}]},\"finishReason\":\"STOP\",\"index\":1}]}\n\n"
	usage := "data: {\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":2,\"totalTokenCount\":5}}\n\n"
	wire := first + second + usage
	for _, requestBody := range []string{
		`{"contents":[],"generationConfig":{"candidateCount":2}}`,
		`{"contents":[],"generation_config":{"candidate_count":2}}`,
		`{"contents":[],"generationConfig":{"candidate_count":2}}`,
		`{"contents":[],"generation_config":{"candidateCount":2}}`,
	} {
		t.Run(requestBody, func(t *testing.T) {
			got, streamErr, providerUsage := collectNativeGeminiStream(t, requestBody, wire)
			if got != wire {
				t.Fatalf("multi-candidate Gemini stream was truncated: got %q, want %q", got, wire)
			}
			if !errors.Is(streamErr, io.EOF) {
				t.Fatalf("multi-candidate Gemini stream did not finish normally: %v", streamErr)
			}
			if providerUsage.PromptTokens != 3 || providerUsage.CompletionTokens != 2 || providerUsage.TotalTokens != 5 || !providerUsage.ProviderReported {
				t.Fatalf("final Gemini usage was lost: %+v", providerUsage)
			}
		})
	}

}

func TestNativeGeminiIncompleteCandidatesKeepTerminalMissing(t *testing.T) {
	for _, test := range []struct {
		name        string
		requestBody string
		wire        string
	}{
		{
			name:        "candidate count two receives one candidate",
			requestBody: `{"contents":[],"generationConfig":{"candidateCount":2}}`,
			wire:        "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"only\"}]},\"finishReason\":\"STOP\",\"index\":0}]}\n\n",
		},
		{
			name:        "ProtoJSON candidate count two receives one candidate",
			requestBody: `{"contents":[],"generation_config":{"candidate_count":2}}`,
			wire:        "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"only\"}]},\"finishReason\":\"STOP\",\"index\":0}]}\n\n",
		},
		{
			name:        "duplicate candidate index does not count twice",
			requestBody: `{"contents":[],"generationConfig":{"candidateCount":2}}`,
			wire: "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"one\"}]},\"finishReason\":\"STOP\",\"index\":0}]}\n\n" +
				"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"repeat\"}]},\"finishReason\":\"STOP\",\"index\":0}]}\n\n",
		},
		{
			name:        "same frame includes unfinished candidate",
			requestBody: `{"contents":[],"generationConfig":{"candidateCount":2}}`,
			wire:        "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"done\"}]},\"finishReason\":\"STOP\",\"index\":0},{\"content\":{\"parts\":[{\"text\":\"partial\"}]},\"index\":1}]}\n\n",
		},
		{
			name:        "default count sees extra unfinished candidate",
			requestBody: `{"contents":[]}`,
			wire:        "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"done\"}]},\"finishReason\":\"STOP\",\"index\":0},{\"content\":{\"parts\":[{\"text\":\"partial\"}]},\"index\":1}]}\n\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, streamErr, _ := collectNativeGeminiStream(t, test.requestBody, test.wire)
			if got != test.wire {
				t.Fatalf("Gemini incomplete stream wire changed: got %q, want %q", got, test.wire)
			}
			if !errors.Is(streamErr, requester.ErrStreamProtocolTerminalMissing) {
				t.Fatalf("Gemini incomplete stream terminal = %v, want %v", streamErr, requester.ErrStreamProtocolTerminalMissing)
			}
		})
	}
}

func TestNativeGeminiSingleCandidateAndInterruptedStreamHaveDifferentTerminals(t *testing.T) {
	for _, test := range []struct {
		name      string
		wire      string
		wantError error
	}{
		{
			name:      "single candidate completes",
			wire:      "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"STOP\",\"index\":0}]}\n\n",
			wantError: io.EOF,
		},
		{
			name:      "unfinished candidate is interrupted",
			wire:      "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"partial\"}]},\"index\":0}]}\n\n",
			wantError: requester.ErrStreamProtocolTerminalMissing,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, streamErr, _ := collectNativeGeminiStream(t, `{"contents":[]}`, test.wire)
			if got != test.wire {
				t.Fatalf("Gemini stream wire changed: got %q, want %q", got, test.wire)
			}
			if !errors.Is(streamErr, test.wantError) {
				t.Fatalf("Gemini stream terminal = %v, want %v", streamErr, test.wantError)
			}
		})
	}
}
