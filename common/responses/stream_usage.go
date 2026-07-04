package responses

import (
	"encoding/json"
	"strings"

	"one-api/types"
)

type StreamUsageEvent struct {
	Type        string                          `json:"type"`
	Delta       json.RawMessage                 `json:"delta,omitempty"`
	Item        *types.ResponsesOutput          `json:"item,omitempty"`
	OutputIndex *int                            `json:"output_index,omitempty"`
	Response    *types.OpenAIResponsesResponses `json:"response,omitempty"`
}

var trackedStreamUsageEvents = map[string]struct{}{
	"response.created":                      {},
	"response.output_text.delta":            {},
	"response.reasoning_summary_text.delta": {},
	"response.output_item.added":            {},
	"response.completed":                    {},
	"response.failed":                       {},
	"response.incomplete":                   {},
	"response.done":                         {},
}

func ParseStreamUsageEvent(payload []byte) (StreamUsageEvent, bool) {
	var meta struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(payload, &meta); err != nil {
		return StreamUsageEvent{}, false
	}
	meta.Type = strings.TrimSpace(meta.Type)
	if _, ok := trackedStreamUsageEvents[meta.Type]; !ok {
		return StreamUsageEvent{}, false
	}

	var event StreamUsageEvent
	if err := json.Unmarshal(payload, &event); err != nil {
		return StreamUsageEvent{}, false
	}
	event.Type = meta.Type
	return event, true
}

func StreamEventDeltaString(delta json.RawMessage) (string, bool) {
	if len(delta) == 0 {
		return "", false
	}
	var text string
	if err := json.Unmarshal(delta, &text); err != nil {
		return "", false
	}
	return text, true
}

func ResponsesSearchType(response *types.OpenAIResponsesResponses) string {
	if response == nil || len(response.Tools) == 0 {
		return ""
	}
	for _, tool := range response.Tools {
		if !types.IsResponsesWebSearchToolType(tool.Type) {
			continue
		}
		if searchType := strings.TrimSpace(tool.SearchContextSize); searchType != "" {
			return searchType
		}
		return "medium"
	}
	return ""
}

func ApplyResponsesOutputItemBilling(usage *types.Usage, item *types.ResponsesOutput, searchType string) {
	if usage == nil || item == nil {
		return
	}
	switch item.Type {
	case types.InputTypeWebSearchCall:
		if searchType == "" {
			searchType = "medium"
		}
		usage.IncExtraBilling(types.APIToolTypeWebSearchPreview, searchType)
	case types.InputTypeCodeInterpreterCall:
		usage.IncExtraBilling(types.APIToolTypeCodeInterpreter, "")
	case types.InputTypeFileSearchCall:
		usage.IncExtraBilling(types.APIToolTypeFileSearch, "")
	case types.InputTypeImageGenerationCall:
		usage.IncExtraBilling(types.APIToolTypeImageGeneration, item.Quality+"-"+item.Size)
	}
}

func ApplyResponsesUsage(usage *types.Usage, response *types.OpenAIResponsesResponses) {
	if usage == nil || response == nil || response.Usage == nil {
		return
	}
	*usage = *response.Usage.ToOpenAIUsage()
	usage.MergeExtraBilling(types.GetResponsesExtraBilling(response))
}
