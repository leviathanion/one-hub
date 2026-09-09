package groq

import (
	"testing"

	"one-api/providers/openai"
	"one-api/types"
)

func TestGroqStreamExtractsXGroqTerminalUsage(t *testing.T) {
	provider := &GroqProvider{}
	provider.Usage = &types.Usage{ResponseModel: "earlier-model"}
	handler := provider.streamHandler(openai.OpenAIStreamHandler{Usage: provider.Usage})
	line := []byte(`data: {"id":"groq-1","model":"earlier-model","service_tier":"flex","choices":[],"x_groq":{"usage":{"prompt_tokens":9,"completion_tokens":0,"total_tokens":9}}}`)
	dataChan := make(chan string, 1)
	errChan := make(chan error, 1)
	handler(&line, dataChan, errChan)

	if !provider.Usage.HasProviderUsage() || provider.Usage.PromptTokens != 9 || provider.Usage.CompletionTokens != 0 {
		t.Fatalf("x_groq terminal usage was not extracted: %+v", provider.Usage)
	}
	if provider.Usage.ResponseModel != "earlier-model" || provider.Usage.ServiceTier != "flex" {
		t.Fatalf("Groq attribution was not preserved: %+v", provider.Usage)
	}
}

func TestGroqCompoundRejectedBeforeProviderWork(t *testing.T) {
	provider := &GroqProvider{}
	_, apiErr := provider.CreateChatCompletion(&types.ChatCompletionRequest{Model: "groq/compound"})
	if apiErr == nil || apiErr.Code != "groq_compound_billing_unsupported" {
		t.Fatalf("expected Compound pre-work rejection, got %+v", apiErr)
	}
}

func TestGroqFactoryRejectsCompoundBeforeProviderConstruction(t *testing.T) {
	if _, err := (GroqProviderFactory{}).AssessChatRequest(nil, "compound-mini", nil); err == nil {
		t.Fatal("factory accepted Groq Compound without billing evidence")
	}
}

func TestGroqPartialXGroqUsageDoesNotAuthorizeTokens(t *testing.T) {
	provider := &GroqProvider{}
	provider.Usage = &types.Usage{}
	handler := provider.streamHandler(openai.OpenAIStreamHandler{Usage: provider.Usage})
	line := []byte(`data: {"id":"groq-2","model":"actual","choices":[],"x_groq":{"usage":{"prompt_tokens":9,"total_tokens":9}}}`)
	handler(&line, make(chan string, 1), make(chan error, 1))
	if provider.Usage.HasProviderUsage() {
		t.Fatalf("partial x_groq usage became provider evidence: %+v", provider.Usage)
	}
}
