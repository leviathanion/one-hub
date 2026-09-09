package openai

import (
	"encoding/json"
	"errors"
	"testing"

	"one-api/model"
	"one-api/providers/base"
	"one-api/types"
)

func TestTranscriptionBillingEvidenceAssessmentMatchesCreateBranch(t *testing.T) {
	for _, test := range []struct {
		format string
		stream bool
		want   bool
	}{
		{format: ""},
		{format: "json"},
		{format: "verbose_json"},
		{format: "diarized_json"},
		{format: "text", want: true},
		{format: "srt", want: true},
		{format: "vtt", want: true},
		{format: "text", stream: true},
		{format: "JSON", want: true},
	} {
		err := ValidateTranscriptionBillingEvidence(&types.AudioRequest{ResponseFormat: test.format, Stream: test.stream})
		var capabilityErr *base.RequestCapabilityError
		if test.want != errors.As(err, &capabilityErr) {
			t.Fatalf("format=%q stream=%t error=%v want rejection=%t", test.format, test.stream, err, test.want)
		}
	}
}

func TestDiarizedJSONClassificationIsScopedToTranscriptions(t *testing.T) {
	request := &types.AudioRequest{ResponseFormat: "diarized_json"}
	if !hasJSONTranscriptionResponse(request) {
		t.Fatal("diarized_json was not accepted as a transcription JSON response")
	}
	if hasJSONResponse(request) {
		t.Fatal("diarized_json unexpectedly broadened the shared translation JSON classifier")
	}
}

func TestOpenAIDialectAssessmentUsesPostCustomParametersForOwnedRules(t *testing.T) {
	search := `{"web_search_options":{}}`
	channel := &model.Channel{CustomParameter: &search}
	err := ValidateChatRequestForChannel(channel, "gpt-5", &types.ChatCompletionRequest{Model: "gpt-5"}, nil)
	var capabilityErr *base.RequestCapabilityError
	if !errors.As(err, &capabilityErr) || capabilityErr.Param != "web_search_options" {
		t.Fatalf("post-custom Chat Search missed billing gate: %v", err)
	}

	tools := `{"tools":[{"type":"function","function":{"name":"lookup"}}]}`
	channel = &model.Channel{CustomParameter: &tools, OnlyChat: true}
	err = ValidateChatRequestForChannel(channel, "gpt-5", &types.ChatCompletionRequest{Model: "gpt-5"}, nil)
	if !errors.As(err, &capabilityErr) || capabilityErr.Param != "tools" {
		t.Fatalf("post-custom OpenAI tools bypassed OnlyChat: %v", err)
	}

	store := `{"store":true}`
	channel = &model.Channel{CustomParameter: &store}
	err = ValidateChatRequestForChannel(channel, "gpt-5", &types.ChatCompletionRequest{Model: "gpt-5"}, nil)
	if !errors.As(err, &capabilityErr) || capabilityErr.Param != "store" {
		t.Fatalf("post-custom store=true bypassed Stored Chat lifecycle gate: %v", err)
	}

	futureUnion := `{"overwrite":true,"modalities":{"future":true}}`
	channel = &model.Channel{CustomParameter: &futureUnion}
	if err := ValidateChatRequestForChannel(channel, "gpt-5", &types.ChatCompletionRequest{Model: "gpt-5"}, nil); err != nil {
		t.Fatalf("owned billing/OnlyChat check decoded an unrelated future union: %v", err)
	}
}

func TestOpenAIPostCustomCannotUseAnUnrepresentableStoreMode(t *testing.T) {
	for _, test := range []struct {
		value string
		allow bool
	}{{"false", true}, {"null", true}, {"true", false}, {`"true"`, false}, {"1", false}, {`{"future":true}`, false}} {
		parameter := `{"store":` + test.value + `}`
		err := ValidateChatRequestForChannel(&model.Channel{CustomParameter: &parameter}, "gpt-5", &types.ChatCompletionRequest{Model: "gpt-5"}, nil)
		if (err == nil) != test.allow {
			t.Fatalf("store=%s allowed=%t err=%v", test.value, test.allow, err)
		}
	}
}

func TestOpenAIDialectAssessmentPreservesExplicitEmptyFieldPresence(t *testing.T) {
	custom := `{"tools":[{"type":"function","function":{"name":"injected"}}],"web_search_options":{"search_context_size":"low"}}`
	channel := &model.Channel{CustomParameter: &custom, OnlyChat: true}
	request := &types.ChatCompletionRequest{Model: "gpt-5", Tools: []*types.ChatCompletionTool{}}
	fields := map[string]json.RawMessage{
		"model":              json.RawMessage(`"gpt-5"`),
		"tools":              json.RawMessage(`[]`),
		"web_search_options": json.RawMessage(`null`),
	}
	if err := ValidateChatRequestForChannel(channel, "gpt-5", request, fields); err != nil {
		t.Fatalf("explicit empty/null fields lost wire presence during owned-key assessment: %v", err)
	}
}
