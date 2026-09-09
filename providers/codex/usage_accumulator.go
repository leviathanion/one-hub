package codex

import (
	"net/http"
	"strings"

	"one-api/common"
	commonresponses "one-api/common/responses"
	"one-api/types"
)

type codexTurnUsageAccumulator struct {
	seedPromptTokens       int
	seedPromptTokenDetails types.PromptTokensDetails
	observedResponsesUsage *types.ResponsesUsage
	searchServiceType      string
	searchType             string
	imageTracker           commonresponses.ImageGenerationStreamTracker
	toolTracker            commonresponses.ToolBillingStreamTracker
	toolUsage              types.Usage
	emittedExtraBilling    map[string]int
	emittedDiagnostics     map[string]bool
}

func newCodexTurnUsageAccumulator() *codexTurnUsageAccumulator {
	return &codexTurnUsageAccumulator{}
}

func (a *codexTurnUsageAccumulator) SeedFromUsage(usage *types.Usage) {
	if a == nil || usage == nil {
		return
	}
	if usage.PromptTokens > 0 {
		a.seedPromptTokens = usage.PromptTokens
	}
	a.seedPromptTokenDetails = usage.PromptTokensDetails
}

func (a *codexTurnUsageAccumulator) SeedPromptFromRequest(request *types.OpenAIResponsesRequest, preCostType int) {
	if a == nil {
		return
	}
	if request == nil {
		return
	}

	modelName := strings.TrimSpace(request.Model)
	if modelName == "" {
		return
	}

	promptTokens := safeCountCodexPromptTokens(request.Input, modelName, preCostType)
	if promptTokens > 0 {
		a.seedPromptTokens = promptTokens
	}
}

func safeCountCodexPromptTokens(input any, modelName string, preCostType int) (tokens int) {
	defer func() {
		if recover() != nil {
			tokens = 0
		}
	}()
	return common.CountTokenInputMessages(input, modelName, preCostType)
}

func (a *codexTurnUsageAccumulator) ObserveEvent(event *types.OpenAIResponsesStreamResponses) error {
	if a == nil || event == nil {
		return nil
	}
	event = normalizeCodexUsageEvent(event)
	// Image item events cannot also change tool state; terminal/created events
	// only change image scalar metadata. Stage that metadata until tool checks
	// succeed, without copying the accumulated per-image maps on every delta.
	candidateImage := a.imageTracker
	candidateSearch := *a
	if event.Type == "response.created" {
		if err := candidateSearch.updateSearchBilling(event.Response); err != nil {
			return err
		}
	}
	if err := candidateImage.ObserveResponsesEvent(event); err != nil {
		return common.ErrorWrapperLocal(err, commonresponses.ResponsesStreamTrackingFailureCode(err), http.StatusBadGateway)
	}

	eventType := strings.TrimSpace(event.Type)
	switch eventType {
	case "response.output_item.done":
		if event.Item != nil && event.Item.Type == types.InputTypeWebSearchCall {
			if err := commonresponses.ApplyResponsesStreamOutputItemBillingWithToolTracker(
				&a.toolUsage, eventType, event.Item, event.ItemID, event.OutputIndex,
				a.searchServiceType, a.searchType, &a.toolTracker,
			); err != nil {
				return common.ErrorWrapperLocal(err, commonresponses.ResponsesStreamTrackingFailureCode(err), http.StatusBadGateway)
			}
		}
	case "response.completed", "response.failed", "response.incomplete":
		serviceType, billingType := commonresponses.ResponsesSearchBilling(event.Response)
		if serviceType == "" {
			serviceType, billingType = a.searchServiceType, a.searchType
		}
		if err := commonresponses.ApplyResponsesTerminalOutputItemBillingWithToolTracker(
			&a.toolUsage, event.Response, serviceType, billingType, &a.toolTracker,
		); err != nil {
			return common.ErrorWrapperLocal(err, commonresponses.ResponsesStreamTrackingFailureCode(err), http.StatusBadGateway)
		}
		imageUsage := &types.Usage{}
		candidateImage.ApplyImageGenerationBilling(event.Response, imageUsage)
		commonresponses.MergeResponsesExtraBillingMax(&a.toolUsage, imageUsage.ExtraBilling)
	}
	a.imageTracker = candidateImage
	a.searchServiceType, a.searchType = candidateSearch.searchServiceType, candidateSearch.searchType
	if event.Response != nil {
		if usage := cloneCodexResponsesUsage(event.Response.Usage); usage != nil {
			a.observedResponsesUsage = usage
		}
	}
	return nil
}

func (a *codexTurnUsageAccumulator) updateSearchBilling(response *types.OpenAIResponsesResponses) error {
	if a == nil {
		return nil
	}
	serviceType, billingType := commonresponses.ResponsesSearchBilling(response)
	if serviceType == "" {
		return nil
	}
	if billingType == "" {
		billingType = "medium"
	}
	serviceType = strings.TrimSpace(serviceType)
	billingType = strings.TrimSpace(billingType)
	if err := commonresponses.ValidateResponsesStreamToolBillingDimensions(serviceType, billingType); err != nil {
		return common.ErrorWrapperLocal(err, commonresponses.ResponsesStreamTrackingFailureCode(err), http.StatusBadGateway)
	}
	// Web search price is keyed by service (and model tier), not context type.
	// The type remains diagnostic; if pricing becomes type-dependent, its
	// aggregate key and settlement contract must change together.
	a.searchServiceType = strings.Clone(serviceType)
	a.searchType = strings.Clone(billingType)
	return nil
}

func normalizeCodexUsageEvent(event *types.OpenAIResponsesStreamResponses) *types.OpenAIResponsesStreamResponses {
	if event == nil || event.Type != types.EventTypeResponseDone {
		return event
	}
	evidence, terminal := interpretCodexSupplierTerminal(event.Type, event.Response)
	if !terminal || evidence.publicEventType == "" {
		return event
	}
	normalized := *event
	normalized.Type = evidence.publicEventType
	if event.Response != nil {
		response := *event.Response
		if strings.TrimSpace(response.Status) == "" {
			response.Status = evidence.responseStatus
		}
		normalized.Response = &response
	}
	return &normalized
}

func (a *codexTurnUsageAccumulator) ResolveUsage(response *types.OpenAIResponsesResponses) *types.Usage {
	if response == nil {
		return nil
	}

	providerUsage := response.Usage != nil
	usageSource := cloneCodexResponsesUsage(response.Usage)
	if usageSource == nil {
		usageSource = cloneCodexResponsesUsage(a.observedResponsesUsage)
		providerUsage = usageSource != nil
	}
	if usageSource == nil {
		usageSource = a.seedResponsesUsage()
	}
	if providerUsage {
		usageSource.MarkProviderReported()
	}
	if usageSource == nil {
		usageSource = &types.ResponsesUsage{}
	}

	response.Usage = usageSource
	resolved := usageSource.ToOpenAIUsage()
	resolved.ResponseModel = response.Model
	resolved.ServiceTier = response.ServiceTier
	resolved.ExtraBilling = cloneCodexExtraBilling(a.toolUsage.ExtraBilling)
	for key, billing := range resolved.ExtraBilling {
		resolved.MarkProviderExtraBilling(key, billing)
	}
	resolved.MergeBillingDiagnostics(a.toolUsage.BillingDiagnostics)
	return resolved
}

func (a *codexTurnUsageAccumulator) ResolveUsageEvent(response *types.OpenAIResponsesResponses) *types.UsageEvent {
	resolved := a.ResolveUsage(response)
	if resolved == nil {
		return nil
	}
	extraBilling := a.takeExtraBillingDelta(resolved.ExtraBilling)
	event := &types.UsageEvent{
		InputTokens:           resolved.PromptTokens,
		OutputTokens:          resolved.CompletionTokens,
		TotalTokens:           resolved.TotalTokens,
		InputTokenDetails:     resolved.PromptTokensDetails,
		OutputTokenDetails:    resolved.CompletionTokensDetails,
		ResponseModel:         resolved.ResponseModel,
		ServiceTier:           resolved.ServiceTier,
		ExtraBilling:          extraBilling,
		BillingDiagnostics:    a.takeBillingDiagnosticsDelta(resolved.BillingDiagnostics),
		ProviderExtraBilling:  providerExtraBillingEvidence(extraBilling),
		ProviderTokenEvidence: resolved.HasProviderUsage(),
	}
	if resolved.ProviderReported {
		event.Source = types.UsageSourceResponsesResponse
	}
	return event
}

func (a *codexTurnUsageAccumulator) BillingUsageEvent() *types.UsageEvent {
	if a == nil || (len(a.toolUsage.ExtraBilling) == 0 && len(a.toolUsage.BillingDiagnostics) == 0) {
		return nil
	}
	extraBilling := a.takeExtraBillingDelta(a.toolUsage.ExtraBilling)
	diagnostics := a.takeBillingDiagnosticsDelta(a.toolUsage.BillingDiagnostics)
	if len(extraBilling) == 0 && len(diagnostics) == 0 {
		return nil
	}
	return &types.UsageEvent{
		ExtraBilling:         extraBilling,
		BillingDiagnostics:   diagnostics,
		ProviderExtraBilling: providerExtraBillingEvidence(extraBilling),
	}
}

func providerExtraBillingEvidence(extraBilling map[string]types.ExtraBilling) map[string]bool {
	if len(extraBilling) == 0 {
		return nil
	}
	evidence := make(map[string]bool, len(extraBilling))
	for key := range extraBilling {
		evidence[key] = true
	}
	return evidence
}

// Provider UsageEvents are merged as increments by the relay. The Codex
// accumulator owns the cumulative supplier evidence, so it converts snapshots
// to deltas exactly once at this boundary.
func (a *codexTurnUsageAccumulator) takeExtraBillingDelta(current map[string]types.ExtraBilling) map[string]types.ExtraBilling {
	if a == nil || len(current) == 0 {
		return nil
	}
	if a.emittedExtraBilling == nil {
		a.emittedExtraBilling = make(map[string]int, len(current))
	}
	var delta map[string]types.ExtraBilling
	for key, billing := range current {
		alreadyEmitted := a.emittedExtraBilling[key]
		if billing.CallCount <= alreadyEmitted {
			continue
		}
		if delta == nil {
			delta = make(map[string]types.ExtraBilling)
		}
		increment := billing
		increment.CallCount -= alreadyEmitted
		delta[key] = increment
		a.emittedExtraBilling[key] = billing.CallCount
	}
	return delta
}

func (a *codexTurnUsageAccumulator) takeBillingDiagnosticsDelta(current map[string]bool) map[string]bool {
	if a == nil || len(current) == 0 {
		return nil
	}
	if a.emittedDiagnostics == nil {
		a.emittedDiagnostics = make(map[string]bool, len(current))
	}
	var delta map[string]bool
	for diagnostic, present := range current {
		if !present || a.emittedDiagnostics[diagnostic] {
			continue
		}
		if delta == nil {
			delta = make(map[string]bool)
		}
		delta[diagnostic] = true
		a.emittedDiagnostics[diagnostic] = true
	}
	return delta
}

func (a *codexTurnUsageAccumulator) seedResponsesUsage() *types.ResponsesUsage {
	if a == nil {
		return nil
	}

	seed := (&types.Usage{
		PromptTokens:        a.seedPromptTokens,
		PromptTokensDetails: a.seedPromptTokenDetails,
	}).ToResponsesUsage()
	seed.OutputTokens = 0
	seed.TotalTokens = seed.InputTokens
	seed.OutputTokensDetails = nil
	return seed
}

func cloneCodexResponsesUsage(usage *types.ResponsesUsage) *types.ResponsesUsage {
	if usage == nil {
		return nil
	}

	cloned := *usage
	if usage.InputTokensDetails != nil {
		details := *usage.InputTokensDetails
		cloned.InputTokensDetails = &details
	}
	if usage.OutputTokensDetails != nil {
		details := *usage.OutputTokensDetails
		cloned.OutputTokensDetails = &details
	}
	return &cloned
}
