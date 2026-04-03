package openai

import (
	runtimesession "one-api/runtime/session"
	"one-api/types"
	"strings"
	"time"
)

type openAIRealtimeTurnState struct {
	seq               int64
	startedAt         time.Time
	firstResponseAt   time.Time
	completedAt       time.Time
	lastResponseID    string
	seenResponseIDs   []string
	terminationReason string
	usageSnapshot     *types.UsageEvent
	accountedUsage    *types.UsageEvent
	observer          runtimesession.TurnObserver
}

func newOpenAIRealtimeTurnState(seq int64, startedAt time.Time, observer runtimesession.TurnObserver) *openAIRealtimeTurnState {
	if startedAt.IsZero() {
		startedAt = time.Now()
	}
	return &openAIRealtimeTurnState{
		seq:       seq,
		startedAt: startedAt,
		observer:  observer,
	}
}

func (t *openAIRealtimeTurnState) observeSupplierEvent(eventType, responseID string, now time.Time, isError bool) {
	if t == nil {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	t.rememberResponseID(responseID)
	if isError || strings.TrimSpace(eventType) == types.EventTypeSessionCreated {
		return
	}
	if t.firstResponseAt.IsZero() {
		t.firstResponseAt = now
	}
}

func (t *openAIRealtimeTurnState) matchesResponseID(responseID string) bool {
	if t == nil {
		return false
	}
	trimmed := strings.TrimSpace(responseID)
	if trimmed == "" {
		return false
	}
	for _, current := range t.seenResponseIDs {
		if current == trimmed {
			return true
		}
	}
	return false
}

func (t *openAIRealtimeTurnState) responseIDs() []string {
	if t == nil || len(t.seenResponseIDs) == 0 {
		return nil
	}
	return append([]string(nil), t.seenResponseIDs...)
}

func (t *openAIRealtimeTurnState) rememberResponseID(responseID string) {
	if t == nil {
		return
	}
	trimmed := strings.TrimSpace(responseID)
	if trimmed == "" {
		return
	}
	t.lastResponseID = trimmed
	for _, current := range t.seenResponseIDs {
		if current == trimmed {
			return
		}
	}
	t.seenResponseIDs = append(t.seenResponseIDs, trimmed)
}

func (t *openAIRealtimeTurnState) applyUsageSnapshot(snapshot *types.UsageEvent) *types.UsageEvent {
	if t == nil || snapshot == nil {
		return nil
	}

	resolved := mergeOpenAIRealtimeUsageSnapshot(t.usageSnapshot, snapshot)
	t.usageSnapshot = resolved

	delta := deltaOpenAIRealtimeUsageSnapshot(resolved, t.accountedUsage)
	if delta != nil {
		t.accountedUsage = resolved.Clone()
	}
	return delta
}

func (t *openAIRealtimeTurnState) finalize(sessionID, model, reason string, now time.Time) (runtimesession.TurnObserver, runtimesession.TurnFinalizePayload) {
	if t == nil {
		return nil, runtimesession.TurnFinalizePayload{}
	}
	if now.IsZero() {
		now = time.Now()
	}
	if t.startedAt.IsZero() {
		t.startedAt = now
	}
	t.completedAt = now
	if trimmed := strings.TrimSpace(reason); trimmed != "" {
		t.terminationReason = trimmed
	}

	return t.observer, runtimesession.TurnFinalizePayload{
		SessionID:         sessionID,
		Model:             model,
		TurnSeq:           t.seq,
		LastResponseID:    t.lastResponseID,
		TerminationReason: t.terminationReason,
		StartedAt:         t.startedAt,
		FirstResponseAt:   t.firstResponseAt,
		CompletedAt:       t.completedAt,
		Usage:             t.usageSnapshot.Clone(),
	}
}

func newOpenAIRealtimeClientError(code, message string) error {
	return types.NewErrorEvent("", "invalid_request_error", strings.TrimSpace(code), message)
}

func mergeOpenAIRealtimeUsageSnapshot(base, update *types.UsageEvent) *types.UsageEvent {
	switch {
	case base == nil:
		return update.Clone()
	case update == nil:
		return base.Clone()
	}

	merged := base.Clone()
	merged.InputTokens = maxOpenAIRealtimeUsageInt(merged.InputTokens, update.InputTokens)
	merged.OutputTokens = maxOpenAIRealtimeUsageInt(merged.OutputTokens, update.OutputTokens)
	merged.TotalTokens = maxOpenAIRealtimeUsageInt(merged.TotalTokens, update.TotalTokens)
	merged.InputTokenDetails = maxOpenAIRealtimePromptTokenDetails(merged.InputTokenDetails, update.InputTokenDetails)
	merged.OutputTokenDetails = maxOpenAIRealtimeCompletionTokenDetails(merged.OutputTokenDetails, update.OutputTokenDetails)
	merged.ExtraTokens = maxOpenAIRealtimeExtraTokens(merged.ExtraTokens, update.ExtraTokens)
	merged.ExtraBilling = maxOpenAIRealtimeExtraBilling(merged.ExtraBilling, update.ExtraBilling)
	return merged
}

func deltaOpenAIRealtimeUsageSnapshot(snapshot, accounted *types.UsageEvent) *types.UsageEvent {
	if snapshot == nil {
		return nil
	}

	delta := &types.UsageEvent{
		InputTokens:        deltaOpenAIRealtimeUsageInt(snapshot.InputTokens, usageEventField(accounted, func(usage *types.UsageEvent) int { return usage.InputTokens })),
		OutputTokens:       deltaOpenAIRealtimeUsageInt(snapshot.OutputTokens, usageEventField(accounted, func(usage *types.UsageEvent) int { return usage.OutputTokens })),
		TotalTokens:        deltaOpenAIRealtimeUsageInt(snapshot.TotalTokens, usageEventField(accounted, func(usage *types.UsageEvent) int { return usage.TotalTokens })),
		InputTokenDetails:  deltaOpenAIRealtimePromptTokenDetails(snapshot.InputTokenDetails, promptTokenDetailsField(accounted)),
		OutputTokenDetails: deltaOpenAIRealtimeCompletionTokenDetails(snapshot.OutputTokenDetails, completionTokenDetailsField(accounted)),
		ExtraTokens:        deltaOpenAIRealtimeExtraTokens(snapshot.ExtraTokens, usageEventExtraTokens(accounted)),
		ExtraBilling:       deltaOpenAIRealtimeExtraBilling(snapshot.ExtraBilling, usageEventExtraBilling(accounted)),
	}
	if !openAIRealtimeUsageHasValue(delta) {
		return nil
	}
	return delta
}

func openAIRealtimeUsageHasValue(usage *types.UsageEvent) bool {
	if usage == nil {
		return false
	}
	if usage.InputTokens > 0 || usage.OutputTokens > 0 || usage.TotalTokens > 0 {
		return true
	}
	if openAIRealtimePromptTokenDetailsHasValue(usage.InputTokenDetails) || openAIRealtimeCompletionTokenDetailsHasValue(usage.OutputTokenDetails) {
		return true
	}
	return len(usage.ExtraTokens) > 0 || len(usage.ExtraBilling) > 0
}

func usageEventField(usage *types.UsageEvent, get func(*types.UsageEvent) int) int {
	if usage == nil || get == nil {
		return 0
	}
	return get(usage)
}

func promptTokenDetailsField(usage *types.UsageEvent) types.PromptTokensDetails {
	if usage == nil {
		return types.PromptTokensDetails{}
	}
	return usage.InputTokenDetails
}

func completionTokenDetailsField(usage *types.UsageEvent) types.CompletionTokensDetails {
	if usage == nil {
		return types.CompletionTokensDetails{}
	}
	return usage.OutputTokenDetails
}

func usageEventExtraTokens(usage *types.UsageEvent) map[string]int {
	if usage == nil {
		return nil
	}
	return usage.ExtraTokens
}

func usageEventExtraBilling(usage *types.UsageEvent) map[string]types.ExtraBilling {
	if usage == nil {
		return nil
	}
	return usage.ExtraBilling
}

func maxOpenAIRealtimeUsageInt(current, next int) int {
	if next > current {
		return next
	}
	return current
}

func deltaOpenAIRealtimeUsageInt(current, previous int) int {
	if current <= previous {
		return 0
	}
	return current - previous
}

func maxOpenAIRealtimePromptTokenDetails(current, next types.PromptTokensDetails) types.PromptTokensDetails {
	return types.PromptTokensDetails{
		AudioTokens:          maxOpenAIRealtimeUsageInt(current.AudioTokens, next.AudioTokens),
		CachedTokens:         maxOpenAIRealtimeUsageInt(current.CachedTokens, next.CachedTokens),
		TextTokens:           maxOpenAIRealtimeUsageInt(current.TextTokens, next.TextTokens),
		ImageTokens:          maxOpenAIRealtimeUsageInt(current.ImageTokens, next.ImageTokens),
		CachedTokensInternal: maxOpenAIRealtimeUsageInt(current.CachedTokensInternal, next.CachedTokensInternal),
		CachedWriteTokens:    maxOpenAIRealtimeUsageInt(current.CachedWriteTokens, next.CachedWriteTokens),
		CachedReadTokens:     maxOpenAIRealtimeUsageInt(current.CachedReadTokens, next.CachedReadTokens),
	}
}

func deltaOpenAIRealtimePromptTokenDetails(current, previous types.PromptTokensDetails) types.PromptTokensDetails {
	return types.PromptTokensDetails{
		AudioTokens:          deltaOpenAIRealtimeUsageInt(current.AudioTokens, previous.AudioTokens),
		CachedTokens:         deltaOpenAIRealtimeUsageInt(current.CachedTokens, previous.CachedTokens),
		TextTokens:           deltaOpenAIRealtimeUsageInt(current.TextTokens, previous.TextTokens),
		ImageTokens:          deltaOpenAIRealtimeUsageInt(current.ImageTokens, previous.ImageTokens),
		CachedTokensInternal: deltaOpenAIRealtimeUsageInt(current.CachedTokensInternal, previous.CachedTokensInternal),
		CachedWriteTokens:    deltaOpenAIRealtimeUsageInt(current.CachedWriteTokens, previous.CachedWriteTokens),
		CachedReadTokens:     deltaOpenAIRealtimeUsageInt(current.CachedReadTokens, previous.CachedReadTokens),
	}
}

func openAIRealtimePromptTokenDetailsHasValue(details types.PromptTokensDetails) bool {
	return details.AudioTokens > 0 ||
		details.CachedTokens > 0 ||
		details.TextTokens > 0 ||
		details.ImageTokens > 0 ||
		details.CachedTokensInternal > 0 ||
		details.CachedWriteTokens > 0 ||
		details.CachedReadTokens > 0
}

func maxOpenAIRealtimeCompletionTokenDetails(current, next types.CompletionTokensDetails) types.CompletionTokensDetails {
	return types.CompletionTokensDetails{
		AudioTokens:              maxOpenAIRealtimeUsageInt(current.AudioTokens, next.AudioTokens),
		TextTokens:               maxOpenAIRealtimeUsageInt(current.TextTokens, next.TextTokens),
		ReasoningTokens:          maxOpenAIRealtimeUsageInt(current.ReasoningTokens, next.ReasoningTokens),
		AcceptedPredictionTokens: maxOpenAIRealtimeUsageInt(current.AcceptedPredictionTokens, next.AcceptedPredictionTokens),
		RejectedPredictionTokens: maxOpenAIRealtimeUsageInt(current.RejectedPredictionTokens, next.RejectedPredictionTokens),
		ImageTokens:              maxOpenAIRealtimeUsageInt(current.ImageTokens, next.ImageTokens),
	}
}

func deltaOpenAIRealtimeCompletionTokenDetails(current, previous types.CompletionTokensDetails) types.CompletionTokensDetails {
	return types.CompletionTokensDetails{
		AudioTokens:              deltaOpenAIRealtimeUsageInt(current.AudioTokens, previous.AudioTokens),
		TextTokens:               deltaOpenAIRealtimeUsageInt(current.TextTokens, previous.TextTokens),
		ReasoningTokens:          deltaOpenAIRealtimeUsageInt(current.ReasoningTokens, previous.ReasoningTokens),
		AcceptedPredictionTokens: deltaOpenAIRealtimeUsageInt(current.AcceptedPredictionTokens, previous.AcceptedPredictionTokens),
		RejectedPredictionTokens: deltaOpenAIRealtimeUsageInt(current.RejectedPredictionTokens, previous.RejectedPredictionTokens),
		ImageTokens:              deltaOpenAIRealtimeUsageInt(current.ImageTokens, previous.ImageTokens),
	}
}

func openAIRealtimeCompletionTokenDetailsHasValue(details types.CompletionTokensDetails) bool {
	return details.AudioTokens > 0 ||
		details.TextTokens > 0 ||
		details.ReasoningTokens > 0 ||
		details.AcceptedPredictionTokens > 0 ||
		details.RejectedPredictionTokens > 0 ||
		details.ImageTokens > 0
}

func maxOpenAIRealtimeExtraTokens(current, next map[string]int) map[string]int {
	if len(next) == 0 {
		return cloneOpenAIRealtimeExtraTokens(current)
	}

	merged := cloneOpenAIRealtimeExtraTokens(current)
	if merged == nil {
		merged = make(map[string]int, len(next))
	}
	for key, value := range next {
		if value > merged[key] {
			merged[key] = value
		}
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}

func deltaOpenAIRealtimeExtraTokens(current, previous map[string]int) map[string]int {
	if len(current) == 0 {
		return nil
	}

	delta := make(map[string]int, len(current))
	for key, value := range current {
		if diff := deltaOpenAIRealtimeUsageInt(value, previous[key]); diff > 0 {
			delta[key] = diff
		}
	}
	if len(delta) == 0 {
		return nil
	}
	return delta
}

func cloneOpenAIRealtimeExtraTokens(extraTokens map[string]int) map[string]int {
	if len(extraTokens) == 0 {
		return nil
	}
	cloned := make(map[string]int, len(extraTokens))
	for key, value := range extraTokens {
		cloned[key] = value
	}
	return cloned
}

func maxOpenAIRealtimeExtraBilling(current, next map[string]types.ExtraBilling) map[string]types.ExtraBilling {
	if len(next) == 0 {
		return cloneOpenAIRealtimeExtraBilling(current)
	}

	merged := cloneOpenAIRealtimeExtraBilling(current)
	if merged == nil {
		merged = make(map[string]types.ExtraBilling, len(next))
	}
	for key, value := range next {
		billing := merged[key]
		if value.CallCount > billing.CallCount {
			billing.CallCount = value.CallCount
		}
		if billing.Type == "" {
			billing.Type = value.Type
		}
		merged[key] = billing
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}

func deltaOpenAIRealtimeExtraBilling(current, previous map[string]types.ExtraBilling) map[string]types.ExtraBilling {
	if len(current) == 0 {
		return nil
	}

	delta := make(map[string]types.ExtraBilling, len(current))
	for key, value := range current {
		prev := previous[key]
		if value.CallCount <= prev.CallCount {
			continue
		}
		delta[key] = types.ExtraBilling{
			Type:      value.Type,
			CallCount: value.CallCount - prev.CallCount,
		}
	}
	if len(delta) == 0 {
		return nil
	}
	return delta
}

func cloneOpenAIRealtimeExtraBilling(extraBilling map[string]types.ExtraBilling) map[string]types.ExtraBilling {
	if len(extraBilling) == 0 {
		return nil
	}
	cloned := make(map[string]types.ExtraBilling, len(extraBilling))
	for key, value := range extraBilling {
		cloned[key] = value
	}
	return cloned
}
