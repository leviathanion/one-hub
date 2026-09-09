package responses

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"

	"one-api/types"
)

type StreamUsageEvent struct {
	Type              string                          `json:"type"`
	Item              *types.ResponsesOutput          `json:"item,omitempty"`
	ItemID            string                          `json:"item_id,omitempty"`
	OutputIndex       *int                            `json:"output_index,omitempty"`
	PartialImageIndex *int                            `json:"partial_image_index,omitempty"`
	Response          *types.OpenAIResponsesResponses `json:"response,omitempty"`
}

// ToolBillingStreamTracker deduplicates cumulative stream evidence for one
// Responses turn. Item IDs are authoritative; output_index is the stable
// fallback used by providers that omit the ID on lifecycle events.
type ToolBillingStreamTracker struct {
	aliasOwners    map[[sha256.Size]byte]int
	entries        []toolBillingIdentity
	anonymousCount int
	maxEntries     int
}

type toolBillingIdentity struct {
	id       [sha256.Size]byte
	callID   [sha256.Size]byte
	index    [sha256.Size]byte
	hasID    bool
	hasCall  bool
	hasIndex bool
}

const maxToolBillingStreamEntries = 1024
const maxToolBillingDimensionBytes = 256

var errToolBillingStreamLimit = errors.New("responses tool billing stream state limit exceeded")
var errToolBillingStreamIdentityConflict = errors.New("responses tool billing stream identity conflict")

var trackedStreamUsageEvents = map[string]struct{}{
	"response.created":                             {},
	"response.output_item.added":                   {},
	"response.output_item.done":                    {},
	"response.image_generation_call.partial_image": {},
	"response.completed":                           {},
	"response.failed":                              {},
	"response.incomplete":                          {},
}

func ParseStreamUsageEvent(payload []byte) (StreamUsageEvent, bool) {
	var wire struct {
		Type              string          `json:"type"`
		Item              json.RawMessage `json:"item"`
		ItemID            string          `json:"item_id"`
		OutputIndex       *int            `json:"output_index"`
		PartialImageIndex *int            `json:"partial_image_index"`
		Response          json.RawMessage `json:"response"`
	}
	if err := json.Unmarshal(payload, &wire); err != nil {
		return StreamUsageEvent{}, false
	}
	wire.Type = strings.TrimSpace(wire.Type)
	if _, ok := trackedStreamUsageEvents[wire.Type]; !ok {
		return StreamUsageEvent{}, false
	}

	event := StreamUsageEvent{
		Type:              wire.Type,
		ItemID:            wire.ItemID,
		OutputIndex:       wire.OutputIndex,
		PartialImageIndex: wire.PartialImageIndex,
	}
	if len(wire.Item) > 0 && !bytes.Equal(bytes.TrimSpace(wire.Item), []byte("null")) {
		event.Item = &types.ResponsesOutput{}
		if err := json.Unmarshal(wire.Item, event.Item); err != nil {
			return StreamUsageEvent{}, false
		}
	}
	if len(wire.Response) > 0 && !bytes.Equal(bytes.TrimSpace(wire.Response), []byte("null")) {
		event.Response = &types.OpenAIResponsesResponses{}
		if err := event.Response.DecodeCapturedProviderJSON(wire.Response); err != nil {
			return StreamUsageEvent{}, false
		}
	}
	return event, true
}

func ResponsesSearchBilling(response *types.OpenAIResponsesResponses) (serviceType, billingType string) {
	return types.ResponsesWebSearchBilling(response)
}

func ApplyResponsesStreamOutputItemBillingWithToolTracker(usage *types.Usage, eventType string, item *types.ResponsesOutput, itemID string, outputIndex *int, searchServiceType, searchType string, toolTracker *ToolBillingStreamTracker) error {
	if usage == nil || item == nil {
		return nil
	}
	if item.Type == types.InputTypeImageGenerationCall {
		// Image generation is committed only by a successful response terminal.
		// The tracker already captured a completed output item and its partials.
		return nil
	}
	switch item.Type {
	case types.InputTypeWebSearchCall:
		// added normally carries in_progress. A completed done event is the first
		// stream fact that proves the hosted search actually ran.
		if eventType != "response.output_item.done" {
			return nil
		}
		bill, unknownAction := types.ShouldBillResponsesWebSearch(*item)
		if !bill {
			return nil
		}
		if searchType == "" {
			searchType = "medium"
		}
		if searchServiceType == "" {
			searchServiceType = types.APIToolTypeWebSearchPreview
		}
		if err := ValidateResponsesStreamToolBillingDimensions(searchServiceType, searchType); err != nil {
			return err
		}
		marked, err := toolTracker.mark(itemID, item, outputIndex)
		if err != nil {
			return err
		}
		if !marked {
			return nil
		}
		if unknownAction {
			usage.AddBillingDiagnostic("web_search_action_unknown")
		}
		usage.IncProviderExtraBilling(searchServiceType, searchType)
	}
	return nil
}

func ValidateResponsesStreamToolBillingDimensions(serviceType, billingType string) error {
	if len(strings.TrimSpace(serviceType)) > maxToolBillingDimensionBytes || len(strings.TrimSpace(billingType)) > maxToolBillingDimensionBytes {
		return errToolBillingStreamLimit
	}
	return nil
}

// ApplyResponsesTerminalOutputItemBillingWithToolTracker treats one terminal
// response as one observation: either all previously unseen tool facts commit,
// or none of that frame changes the tracker or cumulative billing.
func ApplyResponsesTerminalOutputItemBillingWithToolTracker(usage *types.Usage, response *types.OpenAIResponsesResponses, searchServiceType, searchType string, toolTracker *ToolBillingStreamTracker) error {
	if usage == nil || response == nil {
		return nil
	}
	candidateUsage := *usage
	candidateUsage.ExtraBilling = cloneResponsesExtraBilling(usage.ExtraBilling)
	candidateUsage.ProviderExtraBilling = cloneResponsesProviderExtraBilling(usage.ProviderExtraBilling)
	candidateUsage.BillingDiagnostics = cloneResponsesBillingDiagnostics(usage.BillingDiagnostics)

	var candidateTracker *ToolBillingStreamTracker
	if toolTracker != nil {
		cloned := toolTracker.clone()
		candidateTracker = &cloned
	}
	for outputIndex := range response.Output {
		item := &response.Output[outputIndex]
		if err := ApplyResponsesStreamOutputItemBillingWithToolTracker(
			&candidateUsage,
			"response.output_item.done",
			item,
			item.ID,
			&outputIndex,
			searchServiceType,
			searchType,
			candidateTracker,
		); err != nil {
			return err
		}
	}
	usage.ExtraBilling = candidateUsage.ExtraBilling
	usage.ProviderExtraBilling = candidateUsage.ProviderExtraBilling
	usage.BillingDiagnostics = candidateUsage.BillingDiagnostics
	if toolTracker != nil {
		*toolTracker = *candidateTracker
	}
	return nil
}

func (t *ToolBillingStreamTracker) clone() ToolBillingStreamTracker {
	if t == nil {
		return ToolBillingStreamTracker{}
	}
	cloned := *t
	cloned.entries = append([]toolBillingIdentity(nil), t.entries...)
	if t.aliasOwners != nil {
		cloned.aliasOwners = make(map[[sha256.Size]byte]int, len(t.aliasOwners))
		for key, owner := range t.aliasOwners {
			cloned.aliasOwners[key] = owner
		}
	}
	return cloned
}

func (t *ToolBillingStreamTracker) mark(eventItemID string, item *types.ResponsesOutput, outputIndex *int) (bool, error) {
	if t == nil {
		return true, nil
	}
	identity, hasIdentity, err := responsesToolBillingIdentity(eventItemID, item, outputIndex)
	if err != nil {
		return false, err
	}
	if !hasIdentity {
		if t.entryCount() >= t.entryLimit() {
			return false, errToolBillingStreamLimit
		}
		t.anonymousCount++
		return true, nil
	}
	owner := -1
	for _, alias := range identity.aliases() {
		if existing, ok := t.aliasOwners[alias]; ok {
			if owner >= 0 && owner != existing {
				return false, errToolBillingStreamIdentityConflict
			}
			owner = existing
		}
	}
	if owner < 0 {
		if t.entryCount() >= t.entryLimit() {
			return false, errToolBillingStreamLimit
		}
		owner = len(t.entries)
		if t.aliasOwners == nil {
			t.aliasOwners = make(map[[sha256.Size]byte]int, 3)
		}
		t.entries = append(t.entries, identity)
		for _, alias := range identity.aliases() {
			t.aliasOwners[alias] = owner
		}
		return true, nil
	}
	if owner >= len(t.entries) || toolBillingIdentityConflicts(t.entries[owner], identity) {
		return false, errToolBillingStreamIdentityConflict
	}
	merged := mergeToolBillingIdentity(t.entries[owner], identity)
	t.entries[owner] = merged
	for _, alias := range identity.aliases() {
		t.aliasOwners[alias] = owner
	}
	return false, nil
}

func (t *ToolBillingStreamTracker) entryCount() int {
	if t == nil {
		return 0
	}
	return len(t.entries) + t.anonymousCount
}

func (t *ToolBillingStreamTracker) entryLimit() int {
	if t != nil && t.maxEntries > 0 {
		return t.maxEntries
	}
	return maxToolBillingStreamEntries
}

func responsesToolBillingIdentity(eventItemID string, item *types.ResponsesOutput, outputIndex *int) (toolBillingIdentity, bool, error) {
	var identity toolBillingIdentity
	topLevelID := strings.TrimSpace(eventItemID)
	itemID := ""
	if item != nil {
		itemID = strings.TrimSpace(item.ID)
	}
	if topLevelID != "" && itemID != "" && topLevelID != itemID {
		return toolBillingIdentity{}, false, errToolBillingStreamIdentityConflict
	}
	if topLevelID == "" {
		topLevelID = itemID
	}
	if topLevelID != "" {
		identity.id = hashResponsesToolBillingIdentity("id", topLevelID)
		identity.hasID = true
	}
	if item != nil {
		if callID := strings.TrimSpace(item.CallID); callID != "" {
			identity.callID = hashResponsesToolBillingIdentity("call", callID)
			identity.hasCall = true
		}
	}
	if outputIndex != nil {
		identity.index = hashResponsesToolBillingIdentity("index", strconv.Itoa(*outputIndex))
		identity.hasIndex = true
	}
	return identity, identity.hasID || identity.hasCall || identity.hasIndex, nil
}

func (i toolBillingIdentity) aliases() [][sha256.Size]byte {
	aliases := make([][sha256.Size]byte, 0, 3)
	if i.hasID {
		aliases = append(aliases, i.id)
	}
	if i.hasCall {
		aliases = append(aliases, i.callID)
	}
	if i.hasIndex {
		aliases = append(aliases, i.index)
	}
	return aliases
}

func toolBillingIdentityConflicts(existing, incoming toolBillingIdentity) bool {
	return existing.hasID && incoming.hasID && existing.id != incoming.id ||
		existing.hasCall && incoming.hasCall && existing.callID != incoming.callID ||
		existing.hasIndex && incoming.hasIndex && existing.index != incoming.index
}

func mergeToolBillingIdentity(existing, incoming toolBillingIdentity) toolBillingIdentity {
	if incoming.hasID {
		existing.id, existing.hasID = incoming.id, true
	}
	if incoming.hasCall {
		existing.callID, existing.hasCall = incoming.callID, true
	}
	if incoming.hasIndex {
		existing.index, existing.hasIndex = incoming.index, true
	}
	return existing
}

func hashResponsesToolBillingIdentity(kind, value string) [sha256.Size]byte {
	hash := sha256.New()
	_, _ = io.WriteString(hash, kind)
	_, _ = hash.Write([]byte{0})
	_, _ = io.WriteString(hash, value)
	var identity [sha256.Size]byte
	copy(identity[:], hash.Sum(nil))
	return identity
}

func ApplyResponsesUsage(usage *types.Usage, response *types.OpenAIResponsesResponses) {
	ApplyResponsesUsageWithImageTracker(usage, response, nil)
}

func ApplyResponsesUsageWithImageTracker(usage *types.Usage, response *types.OpenAIResponsesResponses, imageTracker *ImageGenerationStreamTracker) {
	if usage == nil || response == nil {
		return
	}
	existingExtraBilling := cloneResponsesExtraBilling(usage.ExtraBilling)
	existingProviderExtraBilling := cloneResponsesProviderExtraBilling(usage.ProviderExtraBilling)
	existingDiagnostics := cloneResponsesBillingDiagnostics(usage.BillingDiagnostics)
	responseModel := usage.ResponseModel
	serviceTier := usage.ServiceTier
	if response.Model != "" {
		responseModel = response.Model
	}
	if response.ServiceTier != "" {
		serviceTier = response.ServiceTier
	}
	if response.Usage == nil {
		usage.ResponseModel = responseModel
		usage.ServiceTier = serviceTier
		terminalBilling := &types.Usage{}
		imageTracker.ApplyExtraBilling(response, terminalBilling)
		mergeResponsesExtraBillingMax(usage, terminalBilling.ExtraBilling)
		mergeResponsesProviderExtraBilling(usage, terminalBilling.ProviderExtraBilling)
		usage.MergeBillingDiagnostics(terminalBilling.BillingDiagnostics)
		return
	}
	resolved := response.Usage.ToOpenAIUsage()
	imageTracker.ApplyExtraBilling(response, resolved)
	mergeResponsesExtraBillingMax(resolved, existingExtraBilling)
	mergeResponsesProviderExtraBilling(resolved, existingProviderExtraBilling)
	resolved.MergeBillingDiagnostics(existingDiagnostics)
	*usage = *resolved
	usage.ResponseModel = responseModel
	usage.ServiceTier = serviceTier
}

func cloneResponsesExtraBilling(source map[string]types.ExtraBilling) map[string]types.ExtraBilling {
	if len(source) == 0 {
		return nil
	}
	cloned := make(map[string]types.ExtraBilling, len(source))
	for key, billing := range source {
		cloned[key] = billing
	}
	return cloned
}

func cloneResponsesProviderExtraBilling(source map[string]bool) map[string]bool {
	if len(source) == 0 {
		return nil
	}
	cloned := make(map[string]bool, len(source))
	for key, present := range source {
		cloned[key] = present
	}
	return cloned
}

func mergeResponsesProviderExtraBilling(usage *types.Usage, source map[string]bool) {
	if usage == nil || len(source) == 0 {
		return
	}
	if usage.ProviderExtraBilling == nil {
		usage.ProviderExtraBilling = make(map[string]bool, len(source))
	}
	for key, present := range source {
		if present && strings.TrimSpace(key) != "" {
			usage.ProviderExtraBilling[key] = true
		}
	}
}

func cloneResponsesBillingDiagnostics(source map[string]bool) map[string]bool {
	if len(source) == 0 {
		return nil
	}
	cloned := make(map[string]bool, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

// Streaming observations and the terminal response are cumulative views of
// the same tool calls. Taking the larger count recovers terminal-only calls
// without charging calls already observed in the stream twice.
func mergeResponsesExtraBillingMax(usage *types.Usage, source map[string]types.ExtraBilling) {
	if usage == nil || len(source) == 0 {
		return
	}
	if usage.ExtraBilling == nil {
		usage.ExtraBilling = make(map[string]types.ExtraBilling, len(source))
	}
	for key, incoming := range source {
		current, exists := usage.ExtraBilling[key]
		if !exists || incoming.CallCount > current.CallCount {
			usage.ExtraBilling[key] = incoming
		}
	}
}

func MergeResponsesExtraBillingMax(usage *types.Usage, source map[string]types.ExtraBilling) {
	mergeResponsesExtraBillingMax(usage, source)
}
