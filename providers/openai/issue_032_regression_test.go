package openai

import (
	"testing"

	"one-api/common/config"
	"one-api/common/wsconn"
	"one-api/types"
)

const issue032TokenTranscriptionPayload = `{"event_id":"evt_token","type":"conversation.item.input_audio_transcription.completed","item_id":"token-item","content_index":0,"usage":{"type":"tokens","input_tokens":13,"output_tokens":9,"total_tokens":22,"input_token_details":{"audio_tokens":13,"text_tokens":0},"output_token_details":{"text_tokens":9}}}`

func TestIssue032RealtimeTokenTranscriptionPreservesCompleteEvidence(t *testing.T) {
	usage := openAIRealtimeInputAudioTranscriptionUsage(types.EventTypeInputAudioTranscriptionCompleted, "evt-override", []byte(issue032TokenTranscriptionPayload))
	if usage == nil {
		t.Fatal("complete token transcription usage was dropped")
	}
	if usage.Source != types.UsageSourceInputAudioTranscription || usage.BillingBasis != types.UsageBillingBasisTokens ||
		usage.ProviderEventID != "evt-override" || usage.ItemID != "token-item" {
		t.Fatalf("token transcription attribution was not retained: %+v", usage)
	}
	if usage.InputTokens != 13 || usage.OutputTokens != 9 || usage.TotalTokens != 22 ||
		usage.InputTokenDetails.AudioTokens != 13 || usage.InputTokenDetails.TextTokens != 0 ||
		usage.OutputTokenDetails.TextTokens != 9 {
		t.Fatalf("token transcription details were not retained: %+v", usage)
	}
	if !usage.ProviderTokenEvidence || usage.ProviderTokenConflict || len(usage.ExtraUsageUnits) != 0 ||
		!usage.ProviderTokenFields["prompt_tokens"] || !usage.ProviderTokenFields["completion_tokens"] ||
		!usage.ProviderTokenFields["total_tokens"] || len(usage.RequiredTokenExtraKeys) != 2 || len(usage.TokenExtraEvidenceGroups) != 1 ||
		len(usage.TokenExtraEvidenceGroups[0]) != 2 ||
		usage.ExtraTokens[config.UsageExtraInputAudio] != 13 || usage.ExtraTokens[config.UsageExtraInputTextTokens] != 0 {
		t.Fatalf("token transcription evidence was not established without a duration unit: %+v", usage)
	}

	projected := usage.ToChatUsage()
	if !projected.ProviderReported || projected.ProviderTokenConflict || projected.PromptTokens != 13 ||
		projected.CompletionTokens != 9 || projected.TotalTokens != 22 ||
		!projected.ProviderTokenFields["prompt_tokens"] || !projected.ProviderTokenFields["completion_tokens"] ||
		!projected.ProviderTokenFields["total_tokens"] || !projected.ProviderTokenFields[config.UsageExtraInputAudio] ||
		!projected.ProviderTokenFields[config.UsageExtraInputTextTokens] {
		t.Fatalf("token evidence did not survive UsageEvent projection: %+v", projected)
	}
}

func TestIssue032RealtimeTokenTranscriptionPreservesOutputDetailsWithoutInputDetails(t *testing.T) {
	payload := []byte(`{"type":"conversation.item.input_audio_transcription.completed","item_id":"output-only-item","usage":{"type":"tokens","input_tokens":13,"output_tokens":9,"total_tokens":22,"output_token_details":{"text_tokens":9}}}`)
	usage := openAIRealtimeInputAudioTranscriptionUsage(types.EventTypeInputAudioTranscriptionCompleted, "", payload)
	if usage == nil || !usage.ProviderTokenEvidence || usage.ProviderTokenConflict || usage.InputTokenDetails.AudioTokens != 0 ||
		usage.OutputTokenDetails.TextTokens != 9 || !usage.ProviderTokenFields[config.UsageExtraOutputTextTokens] ||
		usage.ExtraTokens[config.UsageExtraOutputTextTokens] != 9 {
		t.Fatalf("output details were dropped when input details were omitted: %+v", usage)
	}
	projected := usage.ToChatUsage()
	if projected == nil || projected.CompletionTokensDetails.TextTokens != 9 ||
		!projected.ProviderTokenFields[config.UsageExtraOutputTextTokens] {
		t.Fatalf("output details were lost during UsageEvent projection: %+v", projected)
	}
}

func TestIssue032RealtimeTokenTranscriptionTracksOptionalPartitionsAndRejectsConflicts(t *testing.T) {
	for _, test := range []struct {
		name         string
		payload      string
		wantEvidence bool
		wantConflict bool
	}{
		{
			name:    "missing input",
			payload: `{"type":"conversation.item.input_audio_transcription.completed","item_id":"item","usage":{"type":"tokens","output_tokens":9,"total_tokens":9,"input_token_details":{"audio_tokens":0}}}`,
		},
		{
			name:    "missing output",
			payload: `{"type":"conversation.item.input_audio_transcription.completed","item_id":"item","usage":{"type":"tokens","input_tokens":13,"total_tokens":13,"input_token_details":{"audio_tokens":13}}}`,
		},
		{
			name:    "missing total",
			payload: `{"type":"conversation.item.input_audio_transcription.completed","item_id":"item","usage":{"type":"tokens","input_tokens":13,"output_tokens":9,"input_token_details":{"audio_tokens":13}}}`,
		},
		{
			name:         "conflicting total",
			payload:      `{"type":"conversation.item.input_audio_transcription.completed","item_id":"item","usage":{"type":"tokens","input_tokens":13,"output_tokens":9,"total_tokens":21,"input_token_details":{"audio_tokens":13}}}`,
			wantConflict: true,
		},
		{
			name:         "missing audio partition",
			payload:      `{"type":"conversation.item.input_audio_transcription.completed","item_id":"item","usage":{"type":"tokens","input_tokens":13,"output_tokens":9,"total_tokens":22,"input_token_details":{"text_tokens":13}}}`,
			wantEvidence: true,
		},
		{
			name:         "negative output",
			payload:      `{"type":"conversation.item.input_audio_transcription.completed","item_id":"item","usage":{"type":"tokens","input_tokens":13,"output_tokens":-9,"total_tokens":4,"input_token_details":{"audio_tokens":13}}}`,
			wantConflict: true,
		},
		{
			name:         "negative audio detail",
			payload:      `{"type":"conversation.item.input_audio_transcription.completed","item_id":"item","usage":{"type":"tokens","input_tokens":13,"output_tokens":9,"total_tokens":22,"input_token_details":{"audio_tokens":-1}}}`,
			wantConflict: true,
		},
		{
			name:    "unknown type",
			payload: `{"type":"conversation.item.input_audio_transcription.completed","item_id":"item","usage":{"type":"other","input_tokens":13,"output_tokens":9,"total_tokens":22,"input_token_details":{"audio_tokens":13}}}`,
		},
		{
			name:    "missing usage",
			payload: `{"type":"conversation.item.input_audio_transcription.completed","item_id":"item"}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			usage := openAIRealtimeInputAudioTranscriptionUsage(types.EventTypeInputAudioTranscriptionCompleted, "", []byte(test.payload))
			if test.name == "missing usage" {
				if usage != nil {
					t.Fatalf("missing usage unexpectedly produced an event: %+v", usage)
				}
				return
			}
			if usage == nil {
				t.Fatal("malformed token usage unexpectedly changed the event shape")
			}
			if usage.ProviderTokenEvidence != test.wantEvidence || usage.ProviderIndependentUsageUnits != nil || usage.ExtraUsageUnits != nil {
				t.Fatalf("token usage evidence contract mismatch: %+v", usage)
			}
			if usage.ProviderTokenConflict != test.wantConflict {
				t.Fatalf("token conflict marker=%v want=%v: %+v", usage.ProviderTokenConflict, test.wantConflict, usage)
			}
		})
	}
}

func TestIssue032RealtimeTokenTranscriptionCompletesItsInputOwnerOnce(t *testing.T) {
	s, _, owners := inputWorkFixture(t, false)
	admitTestInput(t, s, "token-item")
	for range 2 {
		outbound, shouldClose := s.observeSupplierMessage(wsconn.TextMessage, []byte(issue032TokenTranscriptionPayload))
		if shouldClose || outbound.err != nil {
			t.Fatalf("token completion failed: outbound=%+v", outbound)
		}
	}
	if len(*owners) != 1 || (*owners)[0].observeCount() != 1 || (*owners)[0].finalizeCount() != 1 {
		t.Fatalf("duplicate token completion reached the owner twice: %+v", *owners)
	}
	usage := (*owners)[0].lastPayload().Usage
	if usage == nil || !usage.ProviderTokenEvidence || usage.ProviderTokenConflict || usage.InputTokens != 13 ||
		usage.OutputTokens != 9 || usage.TotalTokens != 22 || usage.InputTokenDetails.AudioTokens != 13 ||
		usage.ExtraTokens[config.UsageExtraInputAudio] != 13 || usage.ResponseModel != "upstream-transcribe" {
		t.Fatalf("complete token evidence did not survive the input owner: %+v", usage)
	}
}

func TestIssue032RealtimeWrongTokenItemDoesNotFinishAnotherOwner(t *testing.T) {
	s, _, owners := inputWorkFixture(t, false)
	admitTestInput(t, s, "owned-token-item")
	wrong := []byte(`{"type":"conversation.item.input_audio_transcription.completed","item_id":"other-token-item","content_index":0,"usage":{"type":"tokens","input_tokens":13,"output_tokens":9,"total_tokens":22,"input_token_details":{"audio_tokens":13}}}`)
	if outbound, shouldClose := s.observeSupplierMessage(wsconn.TextMessage, wrong); shouldClose || outbound.err != nil {
		t.Fatalf("wrong token item caused session failure: outbound=%+v", outbound)
	}
	if len(*owners) != 1 || (*owners)[0].observeCount() != 0 || (*owners)[0].finalizeCount() != 0 {
		t.Fatalf("wrong token item finished the owned input: %+v", *owners)
	}
}

func TestIssue032RealtimeResponseDoneDoesNotAuthorizeASRTokenEvidence(t *testing.T) {
	if usage := openAIRealtimeInputAudioTranscriptionUsage(types.EventTypeResponseDone, "evt-response", []byte(issue032TokenTranscriptionPayload)); usage != nil {
		t.Fatalf("response.done was accepted as an ASR token completion: %+v", usage)
	}
}
