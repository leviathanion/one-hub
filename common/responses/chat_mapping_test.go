package responses

import (
	"testing"

	"one-api/types"
)

func TestChatTerminalErrorRejectsUnrepresentableFailures(t *testing.T) {
	message := "provider failed"
	code := "provider_failed"
	for _, test := range []struct {
		name     string
		response *types.OpenAIResponsesResponses
		wantErr  bool
	}{
		{name: "completed", response: &types.OpenAIResponsesResponses{Status: types.ResponseStatusCompleted}},
		{name: "length incomplete", response: &types.OpenAIResponsesResponses{Status: types.ResponseStatusIncomplete, IncompleteDetail: &types.IncompleteDetail{Reason: "max_output_tokens"}}},
		{name: "other incomplete", response: &types.OpenAIResponsesResponses{Status: types.ResponseStatusIncomplete, IncompleteDetail: &types.IncompleteDetail{Reason: "content_filter"}}, wantErr: true},
		{name: "failed", response: &types.OpenAIResponsesResponses{Status: types.ResponseStatusFailed, Error: &types.OpenAIError{Message: message, Code: code}}, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			apiErr := ChatTerminalError(test.response)
			if (apiErr != nil) != test.wantErr {
				t.Fatalf("error=%+v wantErr=%v", apiErr, test.wantErr)
			}
			if apiErr != nil && !apiErr.UpstreamAccepted {
				t.Fatalf("post-work terminal error lost accepted fact: %+v", apiErr)
			}
		})
	}
}
