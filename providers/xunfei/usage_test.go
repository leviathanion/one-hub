package xunfei

import (
	"testing"

	"one-api/types"
)

func TestXunfeiUsageIsAuthorizedOnlyByTerminalFrame(t *testing.T) {
	usage := &types.Usage{}
	handler := &xunfeiHandler{Usage: usage}
	finished := false
	nonterminal := []byte(`{"header":{"code":0},"payload":{"choices":{"status":1},"usage":{"text":{"prompt_tokens":3,"completion_tokens":0,"total_tokens":3}}}}`)
	if _, err := handler.handlerData(&nonterminal, &finished); err != nil {
		t.Fatal(err)
	}
	if usage.HasProviderUsage() {
		t.Fatalf("nonterminal Xunfei frame authorized usage: %+v", usage)
	}
	terminal := []byte(`{"header":{"code":0},"payload":{"choices":{"status":2},"usage":{"text":{"prompt_tokens":3,"completion_tokens":0,"total_tokens":3}}}}`)
	if _, err := handler.handlerData(&terminal, &finished); err != nil {
		t.Fatal(err)
	}
	if !usage.HasProviderUsage() || usage.CompletionTokens != 0 {
		t.Fatalf("terminal Xunfei zero-output usage was not authorized: %+v", usage)
	}
	finished = false
	// A second terminal is not reachable in one stream; assess a new request.
	*usage = types.Usage{}
	partial := []byte(`{"header":{"code":0},"payload":{"choices":{"status":2},"usage":{"text":{"prompt_tokens":3,"total_tokens":3}}}}`)
	if _, err := handler.handlerData(&partial, &finished); err != nil {
		t.Fatal(err)
	}
	if usage.HasProviderUsage() {
		t.Fatalf("partial terminal Xunfei usage became provider evidence: %+v", usage)
	}
}
