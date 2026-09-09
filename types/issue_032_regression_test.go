package types

import "testing"

func TestIssue032UsageEventTokenEvidenceSurvivesCopyChain(t *testing.T) {
	event := &UsageEvent{
		InputTokens:  13,
		OutputTokens: 9,
		TotalTokens:  22,
		ProviderTokenFields: map[string]bool{
			"prompt_tokens": true, "completion_tokens": true, "total_tokens": true, "input_audio_tokens": true,
		},
		ProviderTokenEvidence: true,
	}
	event.RequireTokenExtraEvidence("input_audio_tokens", "input_text_tokens")
	event.SetTokenExtraEvidenceGroups([]string{"input_audio_tokens", "input_text_tokens"})

	cloned := event.Clone()
	if cloned == nil || !cloned.ProviderTokenEvidence || !cloned.ProviderTokenFields["input_audio_tokens"] ||
		len(cloned.RequiredTokenExtraKeys) != 2 || len(cloned.TokenExtraEvidenceGroups) != 1 {
		t.Fatalf("token evidence was lost during clone: %+v", cloned)
	}
	cloned.ProviderTokenFields["input_audio_tokens"] = false
	cloned.TokenExtraEvidenceGroups[0][0] = "changed"
	if !event.ProviderTokenFields["input_audio_tokens"] || event.TokenExtraEvidenceGroups[0][0] != "input_audio_tokens" {
		t.Fatalf("token evidence clone shares mutable state: original=%+v clone=%+v", event, cloned)
	}

	merged := &UsageEvent{}
	merged.Merge(event)
	if !merged.ProviderTokenEvidence || !merged.ProviderTokenFields["input_audio_tokens"] || len(merged.TokenExtraEvidenceGroups) != 1 {
		t.Fatalf("token evidence was lost during merge: %+v", merged)
	}
	usage := event.ToChatUsage()
	if usage == nil || !usage.ProviderReported || !usage.ProviderTokenFields["input_audio_tokens"] || len(usage.RequiredTokenExtraKeys) != 2 || len(usage.TokenExtraEvidenceGroups) != 1 {
		t.Fatalf("token evidence was lost converting to Usage: %+v", usage)
	}
}

func TestIssue032UsageEventTokenConflictSurvivesCopyChain(t *testing.T) {
	event := &UsageEvent{
		InputTokens:           13,
		OutputTokens:          9,
		TotalTokens:           21,
		ProviderTokenConflict: true,
	}
	cloned := event.Clone()
	if cloned == nil || !cloned.ProviderTokenConflict {
		t.Fatalf("token conflict was lost during clone: %+v", cloned)
	}

	merged := &UsageEvent{}
	merged.Merge(event)
	if !merged.ProviderTokenConflict {
		t.Fatalf("token conflict was lost during merge: %+v", merged)
	}

	usage := event.ToChatUsage()
	if usage == nil || !usage.ProviderTokenConflict || usage.ProviderReported {
		t.Fatalf("token conflict was not preserved without authorizing usage: %+v", usage)
	}
}
