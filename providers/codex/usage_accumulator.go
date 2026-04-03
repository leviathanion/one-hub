package codex

import (
	"fmt"
	"strings"

	"one-api/common"
	"one-api/types"
)

type codexTurnUsageAccumulator struct {
	seedPromptTokens        int
	seedPromptTokenDetails  types.PromptTokensDetails
	observedResponsesUsage  *types.ResponsesUsage
	searchType              string
	textBuilder             strings.Builder
	extraBilling            map[string]types.ExtraBilling
	countedToolBillingItems map[string]struct{}
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
	if usage.TextBuilder.Len() > 0 {
		a.textBuilder.Reset()
		a.textBuilder.WriteString(usage.TextBuilder.String())
	}
}

func (a *codexTurnUsageAccumulator) SeedPromptFromRequest(request *types.OpenAIResponsesRequest, preCostType int) {
	if a == nil || request == nil {
		return
	}

	modelName := normalizeCodexModelName(request.Model)
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

func (a *codexTurnUsageAccumulator) ObserveEvent(event *types.OpenAIResponsesStreamResponses) {
	if a == nil || event == nil {
		return
	}

	if event.Response != nil {
		if usage := cloneCodexResponsesUsage(event.Response.Usage); usage != nil {
			a.observedResponsesUsage = usage
		}
		if searchType := codexResponsesSearchType(event.Response); searchType != "" {
			a.searchType = searchType
		}
	}

	switch strings.TrimSpace(event.Type) {
	case "response.output_text.delta":
		delta, ok := event.Delta.(string)
		if ok {
			a.textBuilder.WriteString(delta)
		}
	case "response.output_item.added":
		a.observeToolItem(event.Item, event.OutputIndex, 0)
	}
}

func (a *codexTurnUsageAccumulator) ResolveUsage(response *types.OpenAIResponsesResponses, modelName string, allowContentFallback bool) *types.Usage {
	if response == nil {
		return nil
	}

	usageSource := cloneCodexResponsesUsage(response.Usage)
	if usageSource == nil {
		usageSource = cloneCodexResponsesUsage(a.observedResponsesUsage)
	}
	if usageSource == nil {
		usageSource = a.seedResponsesUsage()
	}
	if usageSource == nil {
		usageSource = &types.ResponsesUsage{}
	}

	shouldBackfillContent := allowContentFallback && usageSource.OutputTokens == 0
	if shouldBackfillContent {
		content := response.GetContent()
		if content == "" && a != nil && a.textBuilder.Len() > 0 {
			content = a.textBuilder.String()
		}
		if strings.TrimSpace(content) != "" {
			usageSource.OutputTokens = safeCountCodexResponseTokens(content, modelName)
		}
	}

	if usageSource.TotalTokens == 0 || shouldBackfillContent {
		usageSource.TotalTokens = usageSource.InputTokens + usageSource.OutputTokens
	}

	response.Usage = usageSource
	resolved := usageSource.ToOpenAIUsage()
	resolved.ExtraBilling = resolveCodexExtraBilling(response, a)
	return resolved
}

func (a *codexTurnUsageAccumulator) ResolveUsageEvent(response *types.OpenAIResponsesResponses, modelName string, allowContentFallback bool) *types.UsageEvent {
	resolved := a.ResolveUsage(response, modelName, allowContentFallback)
	if resolved == nil {
		return nil
	}
	return &types.UsageEvent{
		InputTokens:        resolved.PromptTokens,
		OutputTokens:       resolved.CompletionTokens,
		TotalTokens:        resolved.TotalTokens,
		InputTokenDetails:  resolved.PromptTokensDetails,
		OutputTokenDetails: resolved.CompletionTokensDetails,
		ExtraBilling:       cloneCodexExtraBilling(resolved.ExtraBilling),
	}
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

func (a *codexTurnUsageAccumulator) observeToolItem(item *types.ResponsesOutput, outputIndex *int, ordinal int) {
	if a == nil || item == nil {
		return
	}

	key, billingKey, billingType, ok := codexToolBillingDescriptor(item, outputIndex, ordinal, a.searchType)
	if !ok {
		return
	}
	if a.countedToolBillingItems == nil {
		a.countedToolBillingItems = make(map[string]struct{})
	}
	if _, exists := a.countedToolBillingItems[key]; exists {
		return
	}
	a.countedToolBillingItems[key] = struct{}{}
	if a.extraBilling == nil {
		a.extraBilling = make(map[string]types.ExtraBilling)
	}
	serviceType := billingKey
	billingKey = types.BuildExtraBillingKey(serviceType, billingType)
	if billingKey == "" {
		return
	}
	billing := a.extraBilling[billingKey]
	if billing.ServiceType == "" {
		billing.ServiceType = serviceType
	}
	if billing.Type == "" {
		billing.Type = billingType
	}
	billing.CallCount++
	a.extraBilling[billingKey] = billing
}

func resolveCodexExtraBilling(response *types.OpenAIResponsesResponses, accumulator *codexTurnUsageAccumulator) map[string]types.ExtraBilling {
	if accumulator == nil {
		return types.GetResponsesExtraBilling(response)
	}
	if response != nil && accumulator.searchType == "" {
		accumulator.searchType = codexResponsesSearchType(response)
	}

	resolved := cloneCodexExtraBilling(accumulator.extraBilling)
	if response == nil || len(response.Output) == 0 {
		return resolved
	}

	for index := range response.Output {
		accumulator.observeToolItem(&response.Output[index], intPtr(index), index)
	}
	if len(accumulator.extraBilling) == 0 {
		return resolved
	}
	return cloneCodexExtraBilling(accumulator.extraBilling)
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

func codexToolBillingDescriptor(item *types.ResponsesOutput, outputIndex *int, ordinal int, searchType string) (string, string, string, bool) {
	if item == nil {
		return "", "", "", false
	}

	key := codexToolBillingItemKey(item, outputIndex, ordinal)
	switch item.Type {
	case types.InputTypeWebSearchCall:
		if searchType == "" {
			searchType = "medium"
		}
		return key, types.APIToolTypeWebSearchPreview, searchType, true
	case types.InputTypeCodeInterpreterCall:
		return key, types.APIToolTypeCodeInterpreter, "", true
	case types.InputTypeFileSearchCall:
		return key, types.APIToolTypeFileSearch, "", true
	case types.InputTypeImageGenerationCall:
		return key, types.APIToolTypeImageGeneration, item.Quality + "-" + item.Size, true
	default:
		return "", "", "", false
	}
}

func codexToolBillingItemKey(item *types.ResponsesOutput, outputIndex *int, ordinal int) string {
	if item == nil {
		return ""
	}
	if id := strings.TrimSpace(item.ID); id != "" {
		return "id:" + id
	}
	if callID := strings.TrimSpace(item.CallID); callID != "" {
		return "call:" + callID
	}
	if outputIndex != nil {
		return fmt.Sprintf("index:%d:type:%s", *outputIndex, item.Type)
	}
	if name := strings.TrimSpace(item.Name); name != "" {
		return fmt.Sprintf("type:%s:name:%s:ordinal:%d", item.Type, name, ordinal)
	}
	return fmt.Sprintf("type:%s:ordinal:%d", item.Type, ordinal)
}

func intPtr(v int) *int {
	return &v
}
