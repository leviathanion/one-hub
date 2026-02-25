package codex

import (
	"io"
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
