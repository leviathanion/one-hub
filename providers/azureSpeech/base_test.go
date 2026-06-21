package azureSpeech

import (
	"testing"

	"one-api/model"
	"one-api/providers/base"
)

func TestAzureSpeechRequestURLReadsRegionFromJSONOther(t *testing.T) {
	provider := &AzureSpeechProvider{
		BaseProvider: base.BaseProvider{
			Config:  base.ProviderConfig{BaseURL: "https://fallback.example"},
			Channel: &model.Channel{Other: `{"region":"eastasia"}`},
		},
	}

	got := provider.GetFullRequestURL("/cognitiveservices/v1")
	want := "https://eastasia.tts.speech.microsoft.com/cognitiveservices/v1"
	if got != want {
		t.Fatalf("expected region URL %q, got %q", want, got)
	}
}
