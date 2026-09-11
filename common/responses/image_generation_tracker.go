package responses

import (
	"errors"
	"strconv"
	"strings"

	"one-api/types"
)

// ImageGenerationStreamTracker records provider-emitted partial image events.
// The request's partial_images field is only an upper bound; billing evidence
// comes from distinct (item, partial_image_index) events observed on the wire.
type ImageGenerationStreamTracker struct {
	configuredType    string
	partialsByItemID  map[string]map[int]struct{}
	partialsByOutput  map[int]map[int]struct{}
	outputByItemID    map[string]int
	itemIDByOutput    map[int]string
	completedByItemID map[string]imageGenerationOutputFact
	completedByOutput map[int]imageGenerationOutputFact
	terminalEventType string
	observedState     int
	maxState          int
	err               error
}

type imageGenerationOutputFact struct {
	ID      string
	Quality string
	Size    string
}

const (
	maxImageGenerationStreamState         = 8192
	maxImageGenerationIdentifierBytes     = 256
	maxImageGenerationDimensionBytes      = 256
	maxImageGenerationConfiguredTypeBytes = 1024
)

var errImageGenerationStreamLimit = errors.New("responses image generation stream state limit exceeded")
var errImageGenerationStreamIdentityConflict = errors.New("responses image generation stream identity conflict")

func ResponsesStreamTrackingFailureCode(err error) string {
	if errors.Is(err, errImageGenerationStreamIdentityConflict) || errors.Is(err, errToolBillingStreamIdentityConflict) {
		return "provider_protocol_error"
	}
	return "provider_usage_state_limit"
}

// 组件身份冲突是局部计费事实；容量错误仍终止接收。
func ObserveBillingFailure(usage *types.Usage, service string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, errImageGenerationStreamIdentityConflict) || errors.Is(err, errToolBillingStreamIdentityConflict) {
		usage.MarkExtraBillingConflict(service)
		return nil
	}
	return err
}

func (t *ImageGenerationStreamTracker) ObserveUsageEvent(event StreamUsageEvent) error {
	return t.observe(event.Type, event.Response, event.Item, event.ItemID, event.OutputIndex, event.PartialImageIndex)
}

func (t *ImageGenerationStreamTracker) ObserveResponsesEvent(event *types.OpenAIResponsesStreamResponses) error {
	if event == nil {
		return nil
	}
	return t.observe(event.Type, event.Response, event.Item, event.ItemID, event.OutputIndex, event.PartialImageIndex)
}

func (t *ImageGenerationStreamTracker) observe(eventType string, response *types.OpenAIResponsesResponses, item *types.ResponsesOutput, itemID string, outputIndex, partialImageIndex *int) error {
	if t == nil {
		return nil
	}
	if t.err != nil {
		return t.err
	}
	eventType = strings.TrimSpace(eventType)
	switch eventType {
	case "response.failed", "response.incomplete":
		t.terminalEventType = eventType
		return nil
	case "response.completed":
		if !types.ShouldBillResponsesImageGenerationResponse(response) {
			t.terminalEventType = eventType
			return nil
		}
	}

	isPartial := eventType == "response.image_generation_call.partial_image"
	isImageItem := item != nil && item.Type == types.InputTypeImageGenerationCall
	tracksImageItem := isImageItem && (eventType == "response.output_item.added" ||
		(eventType == "response.output_item.done" && types.ShouldBillResponsesImageGeneration(*item)))
	if (eventType == "response.output_item.added" || eventType == "response.output_item.done") && !tracksImageItem {
		return nil
	}

	configuredType := types.ResponsesImageGenerationBillingType(response)
	if len(configuredType) > maxImageGenerationConfiguredTypeBytes {
		return t.fail(errImageGenerationStreamLimit)
	}
	eventItemID := strings.TrimSpace(itemID)
	itemObjectID := ""
	if item != nil {
		itemObjectID = strings.TrimSpace(item.ID)
	}
	responseImageOutputs := 0
	if eventType == "response.completed" && response != nil {
		for outputIndex := range response.Output {
			output := &response.Output[outputIndex]
			if output.Type != types.InputTypeImageGenerationCall || !types.ShouldBillResponsesImageGeneration(*output) {
				continue
			}
			responseImageOutputs++
			if imageGenerationOutputExceedsLimit(output) {
				return t.fail(errImageGenerationStreamLimit)
			}
		}
	}
	if (isPartial || tracksImageItem) && eventItemID != "" && itemObjectID != "" && eventItemID != itemObjectID {
		return t.fail(errImageGenerationStreamIdentityConflict)
	}
	itemID = eventItemID
	if itemID == "" {
		itemID = itemObjectID
	}
	if (isPartial || tracksImageItem) && len(itemID) > maxImageGenerationIdentifierBytes {
		return t.fail(errImageGenerationStreamLimit)
	}
	if tracksImageItem && imageGenerationOutputExceedsLimit(item) {
		return t.fail(errImageGenerationStreamLimit)
	}
	eventCost := responseImageOutputs
	if isPartial || tracksImageItem {
		eventCost++
	}
	if !t.canReserveState(eventCost) {
		return t.fail(errImageGenerationStreamLimit)
	}
	if response != nil && responseImageOutputs > 0 {
		seenItemIDs := make(map[string]struct{}, responseImageOutputs)
		for outputIndex := range response.Output {
			output := &response.Output[outputIndex]
			if output.Type != types.InputTypeImageGenerationCall || !types.ShouldBillResponsesImageGeneration(*output) {
				continue
			}
			outputID := strings.TrimSpace(output.ID)
			if outputID == "" {
				continue
			}
			if _, duplicate := seenItemIDs[outputID]; duplicate || t.bindingConflicts(outputID, &outputIndex) {
				return t.fail(errImageGenerationStreamIdentityConflict)
			}
			seenItemIDs[outputID] = struct{}{}
		}
	}
	if (isPartial || tracksImageItem) && t.bindingConflicts(itemID, outputIndex) {
		return t.fail(errImageGenerationStreamIdentityConflict)
	}
	t.observedState += eventCost
	if configuredType != "" {
		t.configuredType = strings.Clone(configuredType)
	}
	switch eventType {
	case "response.completed", "response.failed", "response.incomplete":
		t.terminalEventType = eventType
	}
	if !isPartial && !tracksImageItem {
		return nil
	}
	itemID = strings.Clone(itemID)
	t.bindItemOutput(itemID, outputIndex)
	if eventType == "response.output_item.done" && tracksImageItem {
		t.rememberCompletedOutput(itemID, outputIndex, item)
	}
	if !isPartial || partialImageIndex == nil || *partialImageIndex < 0 {
		return nil
	}
	if itemID == "" && outputIndex == nil {
		return nil
	}
	if itemID != "" {
		if t.partialsByItemID == nil {
			t.partialsByItemID = make(map[string]map[int]struct{})
		}
		addPartialImageIndex(t.partialsByItemID, itemID, *partialImageIndex)
	}
	if outputIndex != nil {
		if t.partialsByOutput == nil {
			t.partialsByOutput = make(map[int]map[int]struct{})
		}
		addPartialImageIndex(t.partialsByOutput, *outputIndex, *partialImageIndex)
	}
	return nil
}

func (t *ImageGenerationStreamTracker) canReserveState(count int) bool {
	if count <= 0 {
		return true
	}
	limit := maxImageGenerationStreamState
	if t.maxState > 0 {
		limit = t.maxState
	}
	return t.observedState <= limit && count <= limit-t.observedState
}

func imageGenerationOutputExceedsLimit(output *types.ResponsesOutput) bool {
	if output == nil {
		return false
	}
	return len(strings.TrimSpace(output.ID)) > maxImageGenerationIdentifierBytes ||
		len(strings.TrimSpace(output.Quality)) > maxImageGenerationDimensionBytes ||
		len(strings.TrimSpace(output.Size)) > maxImageGenerationDimensionBytes
}

func (t *ImageGenerationStreamTracker) fail(err error) error {
	if t.err == nil {
		t.err = err
	}
	return t.err
}

func (t *ImageGenerationStreamTracker) rememberCompletedOutput(itemID string, outputIndex *int, item *types.ResponsesOutput) {
	if t == nil || item == nil {
		return
	}
	completed := imageGenerationOutputFact{
		ID:      itemID,
		Quality: strings.Clone(strings.TrimSpace(item.Quality)),
		Size:    strings.Clone(strings.TrimSpace(item.Size)),
	}
	if itemID != "" {
		if t.completedByItemID == nil {
			t.completedByItemID = make(map[string]imageGenerationOutputFact)
		}
		t.completedByItemID[itemID] = completed
	}
	if outputIndex != nil {
		if t.completedByOutput == nil {
			t.completedByOutput = make(map[int]imageGenerationOutputFact)
		}
		t.completedByOutput[*outputIndex] = completed
	}
}

func (t *ImageGenerationStreamTracker) bindItemOutput(itemID string, outputIndex *int) {
	if t == nil || itemID == "" || outputIndex == nil {
		return
	}
	if t.outputByItemID == nil {
		t.outputByItemID = make(map[string]int)
	}
	if t.itemIDByOutput == nil {
		t.itemIDByOutput = make(map[int]string)
	}
	t.outputByItemID[itemID] = *outputIndex
	t.itemIDByOutput[*outputIndex] = itemID
}

func (t *ImageGenerationStreamTracker) bindingConflicts(itemID string, outputIndex *int) bool {
	if t == nil || itemID == "" || outputIndex == nil {
		return false
	}
	if existingOutput, exists := t.outputByItemID[itemID]; exists && existingOutput != *outputIndex {
		return true
	}
	if existingItemID, exists := t.itemIDByOutput[*outputIndex]; exists && existingItemID != itemID {
		return true
	}
	return false
}

func addPartialImageIndex[K comparable](partials map[K]map[int]struct{}, key K, partialImageIndex int) {
	indices := partials[key]
	if indices == nil {
		indices = make(map[int]struct{})
		partials[key] = indices
	}
	indices[partialImageIndex] = struct{}{}
}

func (t *ImageGenerationStreamTracker) PartialImageCount(item *types.ResponsesOutput, outputIndex *int) int {
	if t == nil {
		return 0
	}
	itemID := ""
	if item != nil {
		itemID = strings.TrimSpace(item.ID)
	}
	indices := make(map[int]struct{})
	mergePartialImageIndices(indices, t.partialsByItemID[itemID])
	if outputIndex != nil {
		mergePartialImageIndices(indices, t.partialsByOutput[*outputIndex])
		if mappedItemID := t.itemIDByOutput[*outputIndex]; mappedItemID != "" {
			mergePartialImageIndices(indices, t.partialsByItemID[mappedItemID])
		}
	}
	if mappedOutput, ok := t.outputByItemID[itemID]; ok {
		mergePartialImageIndices(indices, t.partialsByOutput[mappedOutput])
	}
	return len(indices)
}

func mergePartialImageIndices(dst, src map[int]struct{}) {
	for index := range src {
		dst[index] = struct{}{}
	}
}

func (t *ImageGenerationStreamTracker) OutputBillingType(item *types.ResponsesOutput, outputIndex *int) string {
	partialImages := t.PartialImageCount(item, outputIndex)
	configuredType := ""
	if t != nil {
		configuredType = t.configuredType
	}
	return types.ResponsesImageGenerationOutputBillingType(configuredType, item, partialImages)
}

func (t *ImageGenerationStreamTracker) ApplyExtraBilling(response *types.OpenAIResponsesResponses, usage *types.Usage) {
	if t == nil {
		types.ApplyResponsesExtraBilling(response, usage)
		return
	}
	imagePartialCount := t.partialImageCountResolver()
	if !t.successfulTerminal(response) {
		imagePartialCount = func(*types.ResponsesOutput, int) int { return -1 }
	}
	types.ApplyResponsesExtraBillingWithImagePartialCounts(response, usage, imagePartialCount)
	t.applyRememberedCompletedOutputs(response, usage)
}

func (t *ImageGenerationStreamTracker) ApplyImageGenerationBilling(response *types.OpenAIResponsesResponses, usage *types.Usage) {
	if t == nil {
		types.ApplyResponsesImageGenerationBillingWithPartialCounts(response, usage, nil)
		return
	}
	if !t.successfulTerminal(response) {
		return
	}
	types.ApplyResponsesImageGenerationBillingWithPartialCounts(response, usage, t.partialImageCountResolver())
	t.applyRememberedCompletedOutputs(response, usage)
}

func (t *ImageGenerationStreamTracker) partialImageCountResolver() func(output *types.ResponsesOutput, outputIndex int) int {
	return func(output *types.ResponsesOutput, outputIndex int) int {
		return t.PartialImageCount(output, &outputIndex)
	}
}

func (t *ImageGenerationStreamTracker) applyRememberedCompletedOutputs(response *types.OpenAIResponsesResponses, usage *types.Usage) {
	if t == nil || usage == nil || !t.successfulTerminal(response) {
		return
	}
	terminalItems := make(map[string]struct{})
	for outputIndex := range response.Output {
		output := &response.Output[outputIndex]
		if output.Type != types.InputTypeImageGenerationCall {
			continue
		}
		t.markOutputAliases(terminalItems, output, &outputIndex)
	}
	for outputIndex, fact := range t.completedByOutput {
		output := fact.output()
		if t.hasOutputAlias(terminalItems, &output, &outputIndex) {
			continue
		}
		usage.IncExtraBilling(types.APIToolTypeImageGeneration, t.OutputBillingType(&output, &outputIndex))
		t.markOutputAliases(terminalItems, &output, &outputIndex)
	}
	for _, fact := range t.completedByItemID {
		output := fact.output()
		if t.hasOutputAlias(terminalItems, &output, nil) {
			continue
		}
		usage.IncExtraBilling(types.APIToolTypeImageGeneration, t.OutputBillingType(&output, nil))
		t.markOutputAliases(terminalItems, &output, nil)
	}
}

func (f imageGenerationOutputFact) output() types.ResponsesOutput {
	return types.ResponsesOutput{
		ID:      f.ID,
		Type:    types.InputTypeImageGenerationCall,
		Status:  "completed",
		Quality: f.Quality,
		Size:    f.Size,
	}
}

func (t *ImageGenerationStreamTracker) successfulTerminal(response *types.OpenAIResponsesResponses) bool {
	if t == nil {
		return types.ShouldBillResponsesImageGenerationResponse(response)
	}
	switch t.terminalEventType {
	case "response.failed", "response.incomplete":
		return false
	case "response.completed":
		return types.ShouldBillResponsesImageGenerationResponse(response)
	default:
		return false
	}
}

func (t *ImageGenerationStreamTracker) outputAliases(output *types.ResponsesOutput, outputIndex *int) []string {
	aliases := make(map[string]struct{})
	itemID := ""
	if output != nil {
		itemID = strings.TrimSpace(output.ID)
		if itemID != "" {
			aliases["id:"+itemID] = struct{}{}
		}
	}
	if outputIndex != nil {
		aliases["index:"+strconv.Itoa(*outputIndex)] = struct{}{}
		if mappedItemID := t.itemIDByOutput[*outputIndex]; mappedItemID != "" {
			aliases["id:"+mappedItemID] = struct{}{}
		}
	}
	if mappedOutput, ok := t.outputByItemID[itemID]; ok {
		aliases["index:"+strconv.Itoa(mappedOutput)] = struct{}{}
	}
	resolved := make([]string, 0, len(aliases))
	for alias := range aliases {
		resolved = append(resolved, alias)
	}
	return resolved
}

func (t *ImageGenerationStreamTracker) hasOutputAlias(seen map[string]struct{}, output *types.ResponsesOutput, outputIndex *int) bool {
	for _, alias := range t.outputAliases(output, outputIndex) {
		if _, exists := seen[alias]; exists {
			return true
		}
	}
	return false
}

func (t *ImageGenerationStreamTracker) markOutputAliases(seen map[string]struct{}, output *types.ResponsesOutput, outputIndex *int) {
	for _, alias := range t.outputAliases(output, outputIndex) {
		seen[alias] = struct{}{}
	}
}
