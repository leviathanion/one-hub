package types

import (
	"encoding/json"
	"fmt"
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
	u.ExtraTokens = fillExtraTokensFromDetails(u.ExtraTokens, u.InputTokenDetails, u.OutputTokenDetails)
	return u.ExtraTokens
}

func (u *UsageEvent) SetExtraTokens(key string, value int) {
	if u.ExtraTokens == nil {
		u.ExtraTokens = make(map[string]int)
	}

	u.ExtraTokens[key] = value
}

func (u *UsageEvent) MergeExtraBilling(extraBilling map[string]ExtraBilling) {
	u.ExtraBilling = mergeExtraBillingMap(u.ExtraBilling, extraBilling)
}

func (u *UsageEvent) IncExtraBilling(key string, bType string) {
	u.ExtraBilling = incExtraBillingMap(u.ExtraBilling, key, bType)
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
