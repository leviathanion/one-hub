package moonshot

import (
	"testing"

	"one-api/types"
)

func TestMoonshotStreamAlwaysRequestsProviderTerminalUsage(t *testing.T) {
	original := &types.StreamOptions{IncludeUsage: false}
	request := &types.ChatCompletionRequest{StreamOptions: original}
	if saved := forceMoonshotProviderUsage(request); saved != original {
		t.Fatal("Moonshot stream usage helper did not preserve the client option")
	}
	if request.StreamOptions == nil || !request.StreamOptions.IncludeUsage {
		t.Fatalf("Moonshot provider request did not force terminal usage: %+v", request.StreamOptions)
	}
}
