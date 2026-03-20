package codex

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"one-api/types"
)

type fakeStringStream struct {
	dataChan chan string
	errChan  chan error
}

func (s *fakeStringStream) Recv() (<-chan string, <-chan error) {
	return s.dataChan, s.errChan
}

func (s *fakeStringStream) Close() {}

func TestCollectResponsesStreamResponseAcceptsDataWithoutSpace(t *testing.T) {
	provider := &CodexProvider{}
	provider.Usage = &types.Usage{}

	stream := &fakeStringStream{
		dataChan: make(chan string),
		errChan:  make(chan error),
	}

	go func() {
		stream.dataChan <- "event: response.completed\ndata:{\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":5,\"total_tokens\":8}}}\n"
		stream.errChan <- io.EOF
	}()

	resp, errWithCode := provider.collectResponsesStreamResponse(stream)
	if errWithCode != nil {
		t.Fatalf("collectResponsesStreamResponse returned error: %v", errWithCode.Message)
	}

	if resp == nil || resp.ID != "resp_1" {
		t.Fatalf("unexpected response: %#v", resp)
	}

	if provider.Usage.TotalTokens != 8 {
		t.Fatalf("expected usage total tokens to be updated, got %d", provider.Usage.TotalTokens)
	}
}

func TestCollectResponsesStreamResponsePreservesEmptyReasoningSummary(t *testing.T) {
	provider := &CodexProvider{}
	provider.Usage = &types.Usage{}

	stream := &fakeStringStream{
		dataChan: make(chan string),
		errChan:  make(chan error),
	}

	go func() {
		stream.dataChan <- "event: response.completed\ndata:{\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"output\":[{\"type\":\"reasoning\",\"id\":\"rs_1\",\"status\":\"completed\",\"summary\":[]}],\"usage\":{\"input_tokens\":3,\"output_tokens\":5,\"total_tokens\":8}}}\n"
		stream.errChan <- io.EOF
	}()

	resp, errWithCode := provider.collectResponsesStreamResponse(stream)
	if errWithCode != nil {
		t.Fatalf("collectResponsesStreamResponse returned error: %v", errWithCode.Message)
	}

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("unexpected marshal error: %v", err)
	}

	if !strings.Contains(string(data), "\"summary\":[]") {
		t.Fatalf("expected marshaled response to preserve empty summary array, got %s", string(data))
	}
}

func TestAdaptCodexCLIAppliesMinimalDefaultInstructions(t *testing.T) {
	provider := &CodexProvider{}
	request := &types.OpenAIResponsesRequest{
		Model:           "gpt-5",
		Instructions:    "",
		MaxOutputTokens: 512,
		Temperature:     ptrFloat64(0.7),
		TopP:            ptrFloat64(0.9),
		Input: []types.InputResponses{
			{
				Type: types.InputTypeMessage,
				Role: types.ChatMessageRoleUser,
				Content: []types.ContentResponses{
					{Type: types.ContentTypeInputText, Text: "hello"},
				},
			},
		},
	}

	provider.adaptCodexCLI(request)

	if request.Instructions != CodexCLIInstructions {
		t.Fatalf("expected minimal default instructions %q, got %q", CodexCLIInstructions, request.Instructions)
	}
	if request.MaxOutputTokens != 0 {
		t.Fatalf("expected max_output_tokens to be cleared, got %d", request.MaxOutputTokens)
	}
	if request.Temperature != nil {
		t.Fatalf("expected temperature to be cleared")
	}
	if request.TopP != nil {
		t.Fatalf("expected top_p to be cleared")
	}
}

func ptrFloat64(v float64) *float64 {
	return &v
}
