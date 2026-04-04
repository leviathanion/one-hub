package types

import (
	"encoding/json"
	"fmt"
	"one-api/common/config"
	"one-api/common/utils"
)

const (
	EventTypeResponseDone   = "response.done"
	EventTypeSessionCreated = "session.created"
	EventTypeError          = "error"
)

type Event struct {
	EventId     string         `json:"event_id"`
	Type        string         `json:"type"`
	Response    *ResponseEvent `json:"response,omitempty"`
	Session     *SessionEvent  `json:"session,omitempty"`
	ErrorDetail *EventError    `json:"error,omitempty"`
}

type EventError struct {
	OpenAIError
	EventId string `json:"event_id"`
}

type SessionEvent struct {
	ID string `json:"id"`
}

func NewErrorEvent(eventId, errType, code, message string) *Event {
	if eventId == "" {
		eventId = fmt.Sprintf("event_%d", utils.GetRandomInt(3))
	}

	return &Event{
		EventId: eventId,
		Type:    EventTypeError,
		ErrorDetail: &EventError{
			EventId: eventId,
			OpenAIError: OpenAIError{
				Type:    errType,
				Code:    code,
				Message: message,
			},
		},
	}
}

func NewSessionCreatedEvent(eventId, sessionID string) *Event {
	if eventId == "" {
		eventId = fmt.Sprintf("event_%d", utils.GetRandomInt(3))
	}

	return &Event{
		EventId: eventId,
		Type:    EventTypeSessionCreated,
		Session: &SessionEvent{
			ID: sessionID,
		},
	}
}

func (e *Event) IsError() bool {
	return e.Type == EventTypeError
}

func (e *Event) Error() string {
	if e.ErrorDetail == nil {
		return ""
	}

	// 转换成JSON
	jsonBytes, err := json.Marshal(e)
	if err != nil {
		return ""
	}
	return string(jsonBytes)
}

type ResponseEvent struct {
	ID     string      `json:"id"`
	Object string      `json:"object"`
	Status string      `json:"status"`
	Usage  *UsageEvent `json:"usage,omitempty"`
}

type UsageEvent struct {
	InputTokens        int                     `json:"input_tokens"`
	OutputTokens       int                     `json:"output_tokens"`
	TotalTokens        int                     `json:"total_tokens"`
	InputTokenDetails  PromptTokensDetails     `json:"input_token_details,omitempty"`
	OutputTokenDetails CompletionTokensDetails `json:"output_token_details,omitempty"`

	ExtraTokens  map[string]int          `json:"-"`
	ExtraBilling map[string]ExtraBilling `json:"-"`
}

func (u *UsageEvent) Clone() *UsageEvent {
	if u == nil {
		return nil
	}

	cloned := *u
	cloned.ExtraTokens = cloneExtraTokensMap(u.ExtraTokens)
	cloned.ExtraBilling = cloneExtraBillingMap(u.ExtraBilling)
	return &cloned
}

func (u *UsageEvent) GetExtraTokens() map[string]int {
	if u.ExtraTokens == nil {
		u.ExtraTokens = make(map[string]int)
	}

	// 组装，已有的数据
	if u.InputTokenDetails.CachedTokens > 0 && u.ExtraTokens[config.UsageExtraCache] == 0 {
		u.ExtraTokens[config.UsageExtraCache] = u.InputTokenDetails.CachedTokens
	}

	if u.InputTokenDetails.AudioTokens > 0 && u.ExtraTokens[config.UsageExtraInputAudio] == 0 {
		u.ExtraTokens[config.UsageExtraInputAudio] = u.InputTokenDetails.AudioTokens
	}

	if u.InputTokenDetails.TextTokens > 0 && u.ExtraTokens[config.UsageExtraInputTextTokens] == 0 {
		u.ExtraTokens[config.UsageExtraInputTextTokens] = u.InputTokenDetails.TextTokens
	}

	if u.InputTokenDetails.CachedWriteTokens > 0 && u.ExtraTokens[config.UsageExtraCachedWrite] == 0 {
		u.ExtraTokens[config.UsageExtraCachedWrite] = u.InputTokenDetails.CachedWriteTokens
	}

	if u.InputTokenDetails.CachedReadTokens > 0 && u.ExtraTokens[config.UsageExtraCachedRead] == 0 {
		u.ExtraTokens[config.UsageExtraCachedRead] = u.InputTokenDetails.CachedReadTokens
	}

	if u.InputTokenDetails.ImageTokens > 0 && u.ExtraTokens[config.UsageExtraInputImageTokens] == 0 {
		u.ExtraTokens[config.UsageExtraInputImageTokens] = u.InputTokenDetails.ImageTokens
	}

	if u.OutputTokenDetails.AudioTokens > 0 && u.ExtraTokens[config.UsageExtraOutputAudio] == 0 {
		u.ExtraTokens[config.UsageExtraOutputAudio] = u.OutputTokenDetails.AudioTokens
	}

	if u.OutputTokenDetails.TextTokens > 0 && u.ExtraTokens[config.UsageExtraOutputTextTokens] == 0 {
		u.ExtraTokens[config.UsageExtraOutputTextTokens] = u.OutputTokenDetails.TextTokens
	}

	if u.OutputTokenDetails.ReasoningTokens > 0 && u.ExtraTokens[config.UsageExtraReasoning] == 0 {
		u.ExtraTokens[config.UsageExtraReasoning] = u.OutputTokenDetails.ReasoningTokens
	}

	if u.OutputTokenDetails.ImageTokens > 0 && u.ExtraTokens[config.UsageExtraOutputImageTokens] == 0 {
		u.ExtraTokens[config.UsageExtraOutputImageTokens] = u.OutputTokenDetails.ImageTokens
	}

	return u.ExtraTokens
}

func (u *UsageEvent) SetExtraTokens(key string, value int) {
	if u.ExtraTokens == nil {
		u.ExtraTokens = make(map[string]int)
	}

	u.ExtraTokens[key] = value
}

func (u *UsageEvent) MergeExtraBilling(extraBilling map[string]ExtraBilling) {
	if len(extraBilling) == 0 {
		return
	}
	if u.ExtraBilling == nil {
		u.ExtraBilling = make(map[string]ExtraBilling, len(extraBilling))
	}
	for key, value := range extraBilling {
		serviceType := ResolveExtraBillingServiceType(key, value)
		bType := ResolveExtraBillingType(key, value)
		key = BuildExtraBillingKey(serviceType, bType)
		if key == "" {
			continue
		}
		billing := u.ExtraBilling[key]
		if billing.ServiceType == "" {
			billing.ServiceType = serviceType
		}
		if billing.Type == "" {
			billing.Type = bType
		}
		billing.CallCount += value.CallCount
		u.ExtraBilling[key] = billing
	}
}

func (u *UsageEvent) IncExtraBilling(key string, bType string) {
	key = BuildExtraBillingKey(key, bType)
	if key == "" {
		return
	}
	if u.ExtraBilling == nil {
		u.ExtraBilling = make(map[string]ExtraBilling)
	}

	billing := u.ExtraBilling[key]
	if billing.ServiceType == "" {
		billing.ServiceType = ResolveExtraBillingServiceType(key, billing)
	}
	if billing.Type == "" {
		billing.Type = ResolveExtraBillingType(key, ExtraBilling{Type: bType})
	}
	billing.CallCount++
	u.ExtraBilling[key] = billing
}

func (u *UsageEvent) ToChatUsage() *Usage {
	return &Usage{
		PromptTokens:            u.InputTokens,
		CompletionTokens:        u.OutputTokens,
		TotalTokens:             u.TotalTokens,
		PromptTokensDetails:     u.InputTokenDetails,
		CompletionTokensDetails: u.OutputTokenDetails,
		ExtraTokens:             cloneExtraTokensMap(u.ExtraTokens),
		ExtraBilling:            cloneExtraBillingMap(u.ExtraBilling),
	}
}

func (u *UsageEvent) Merge(other *UsageEvent) {
	if other == nil {
		return
	}

	u.InputTokens += other.InputTokens
	u.OutputTokens += other.OutputTokens
	u.TotalTokens += other.TotalTokens

	u.InputTokenDetails.Merge(&other.InputTokenDetails)
	u.OutputTokenDetails.Merge(&other.OutputTokenDetails)
	u.ExtraTokens = mergeExtraTokensMap(u.ExtraTokens, other.ExtraTokens)
	u.MergeExtraBilling(other.ExtraBilling)
}
