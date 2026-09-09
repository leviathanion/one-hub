package types

import (
	"encoding/json"
	"fmt"
	"math"
	"one-api/common/utils"
	"strings"
)

const (
	EventTypeResponseDone                     = "response.done"
	EventTypeSessionCreated                   = "session.created"
	EventTypeInputAudioTranscriptionCompleted = "conversation.item.input_audio_transcription.completed"
	EventTypeError                            = "error"
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
	ID    string `json:"id"`
	Model string `json:"model,omitempty"`
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
	ID          string      `json:"id"`
	Object      string      `json:"object"`
	Status      string      `json:"status"`
	Model       string      `json:"model,omitempty"`
	ServiceTier string      `json:"service_tier,omitempty"`
	Usage       *UsageEvent `json:"usage,omitempty"`
}

type UsageSource string

const (
	UsageSourceRealtimeResponse        UsageSource = "realtime_response"
	UsageSourceResponsesResponse       UsageSource = "responses_response"
	UsageSourceInputAudioTranscription UsageSource = "input_audio_transcription"
)

type UsageBillingBasis string

const (
	UsageBillingBasisTokens   UsageBillingBasis = "tokens"
	UsageBillingBasisDuration UsageBillingBasis = "duration"
)

type UsageEvent struct {
	InputTokens        int                     `json:"input_tokens"`
	OutputTokens       int                     `json:"output_tokens"`
	TotalTokens        int                     `json:"total_tokens"`
	InputTokenDetails  PromptTokensDetails     `json:"input_token_details,omitempty"`
	OutputTokenDetails CompletionTokensDetails `json:"output_token_details,omitempty"`
	Source             UsageSource             `json:"source,omitempty"`
	BillingBasis       UsageBillingBasis       `json:"billing_basis,omitempty"`
	ProviderEventID    string                  `json:"provider_event_id,omitempty"`
	ResponseID         string                  `json:"response_id,omitempty"`
	ItemID             string                  `json:"item_id,omitempty"`
	DurationSeconds    float64                 `json:"duration_seconds,omitempty"`
	ResponseModel      string                  `json:"response_model,omitempty"`
	ServiceTier        string                  `json:"service_tier,omitempty"`
	Speed              string                  `json:"-"`
	SpeedConflict      bool                    `json:"-"`

	ExtraTokens                   map[string]int          `json:"-"`
	ExtraUsageUnits               map[string]float64      `json:"-"`
	ExtraBilling                  map[string]ExtraBilling `json:"-"`
	BillingDiagnostics            map[string]bool         `json:"-"`
	ProviderExtraBilling          map[string]bool         `json:"-"`
	ProviderIndependentUsageUnits map[string]bool         `json:"-"`
	ProviderTokenFields           map[string]bool         `json:"-"`
	RequiredTokenExtraKeys        []string                `json:"-"`
	TokenExtraEvidenceGroups      [][]string              `json:"-"`
	ProviderTokenEvidence         bool                    `json:"-"`
	ProviderTokenConflict         bool                    `json:"-"`
	// ProviderOperationUnits is an internal, provider-local completion
	// fact. It is never part of the client wire or inferred from Source.
	ProviderOperationUnits *int `json:"-"`
	AttributionConflict    bool `json:"-"`
}

func (u *UsageEvent) Clone() *UsageEvent {
	if u == nil {
		return nil
	}

	cloned := *u
	cloned.ExtraTokens = cloneExtraTokensMap(u.ExtraTokens)
	cloned.ExtraUsageUnits = cloneExtraUsageUnitsMap(u.ExtraUsageUnits)
	cloned.ExtraBilling = cloneExtraBillingMap(u.ExtraBilling)
	cloned.BillingDiagnostics = cloneBillingDiagnostics(u.BillingDiagnostics)
	cloned.ProviderExtraBilling = cloneBillingDiagnostics(u.ProviderExtraBilling)
	cloned.ProviderIndependentUsageUnits = cloneBillingDiagnostics(u.ProviderIndependentUsageUnits)
	cloned.ProviderTokenFields = cloneBillingDiagnostics(u.ProviderTokenFields)
	cloned.RequiredTokenExtraKeys = cloneStringSlice(u.RequiredTokenExtraKeys)
	cloned.TokenExtraEvidenceGroups = cloneStringGroups(u.TokenExtraEvidenceGroups)
	if u.ProviderOperationUnits != nil {
		count := *u.ProviderOperationUnits
		cloned.ProviderOperationUnits = &count
	}
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

func (u *UsageEvent) SetExtraUsageUnits(key string, value float64) {
	if u == nil || value <= 0 {
		return
	}
	if u.ExtraUsageUnits == nil {
		u.ExtraUsageUnits = make(map[string]float64)
	}
	u.ExtraUsageUnits[key] = value
}

func (u *UsageEvent) MarkProviderTokenField(key string) {
	if u == nil {
		return
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return
	}
	if u.ProviderTokenFields == nil {
		u.ProviderTokenFields = make(map[string]bool)
	}
	u.ProviderTokenFields[key] = true
}

func (u *UsageEvent) RequireTokenExtraEvidence(keys ...string) {
	if u == nil || len(keys) == 0 {
		return
	}
	u.RequiredTokenExtraKeys = mergeStringSlice(u.RequiredTokenExtraKeys, keys)
}

func (u *UsageEvent) SetTokenExtraEvidenceGroups(groups ...[]string) {
	if u == nil {
		return
	}
	u.TokenExtraEvidenceGroups = cloneStringGroups(groups)
}

func (u *UsageEvent) SetProviderIndependentUsageUnit(key string, value float64) {
	if u == nil || value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return
	}
	if u.ExtraUsageUnits == nil {
		u.ExtraUsageUnits = make(map[string]float64)
	}
	if u.ProviderIndependentUsageUnits == nil {
		u.ProviderIndependentUsageUnits = make(map[string]bool)
	}
	u.ExtraUsageUnits[key] = value
	u.ProviderIndependentUsageUnits[key] = true
}

// MarkProviderOperationUnits records a trusted provider completion count.
// Adapters must call this only after validating the provider event and its
// ownership; the field is intentionally excluded from JSON wire output.
func (u *UsageEvent) MarkProviderOperationUnits(count int) {
	if u == nil || count < 0 {
		return
	}
	u.ProviderOperationUnits = new(int)
	*u.ProviderOperationUnits = count
}

func (u *UsageEvent) MergeExtraBilling(extraBilling map[string]ExtraBilling) {
	u.ExtraBilling = mergeExtraBillingMap(u.ExtraBilling, extraBilling)
}

func (u *UsageEvent) IncExtraBilling(key string, bType string) {
	u.ExtraBilling = incExtraBillingMap(u.ExtraBilling, key, bType)
}

func (u *UsageEvent) ToChatUsage() *Usage {
	usage := &Usage{
		PromptTokens:                  u.InputTokens,
		CompletionTokens:              u.OutputTokens,
		TotalTokens:                   u.TotalTokens,
		PromptTokensDetails:           u.InputTokenDetails,
		CompletionTokensDetails:       u.OutputTokenDetails,
		ExtraTokens:                   cloneExtraTokensMap(u.ExtraTokens),
		ExtraUsageUnits:               cloneExtraUsageUnitsMap(u.ExtraUsageUnits),
		ExtraBilling:                  cloneExtraBillingMap(u.ExtraBilling),
		BillingDiagnostics:            cloneBillingDiagnostics(u.BillingDiagnostics),
		ResponseModel:                 u.ResponseModel,
		ServiceTier:                   u.ServiceTier,
		Speed:                         u.Speed,
		SpeedConflict:                 u.SpeedConflict,
		ProviderExtraBilling:          providerExtraBillingFromUsageEvent(u),
		ProviderIndependentUsageUnits: cloneBillingDiagnostics(u.ProviderIndependentUsageUnits),
		ProviderTokenFields:           cloneBillingDiagnostics(u.ProviderTokenFields),
		RequiredTokenExtraKeys:        cloneStringSlice(u.RequiredTokenExtraKeys),
		TokenExtraEvidenceGroups:      cloneStringGroups(u.TokenExtraEvidenceGroups),
		AttributionConflict:           u.AttributionConflict,
		ProviderTokenConflict:         u.ProviderTokenConflict,
	}
	if u.ProviderOperationUnits != nil {
		count := *u.ProviderOperationUnits
		usage.ProviderOperationUnits = &count
	}
	if u.ProviderTokenEvidence {
		usage.MarkProviderReported()
	}
	return usage
}

func providerExtraBillingFromUsageEvent(u *UsageEvent) map[string]bool {
	if u == nil {
		return nil
	}
	return cloneBillingDiagnostics(u.ProviderExtraBilling)
}

func cloneStringSlice(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	return append([]string(nil), values...)
}

func mergeStringSlice(dst, src []string) []string {
	if len(src) == 0 {
		return dst
	}
	result := cloneStringSlice(dst)
	seen := make(map[string]struct{}, len(result)+len(src))
	for _, value := range result {
		value = strings.TrimSpace(value)
		if value != "" {
			seen[value] = struct{}{}
		}
	}
	for _, value := range src {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func cloneStringGroups(groups [][]string) [][]string {
	if len(groups) == 0 {
		return nil
	}
	result := make([][]string, 0, len(groups))
	for _, group := range groups {
		copied := make([]string, 0, len(group))
		seen := make(map[string]struct{}, len(group))
		for _, value := range group {
			value = strings.TrimSpace(value)
			if value == "" {
				continue
			}
			if _, exists := seen[value]; exists {
				continue
			}
			seen[value] = struct{}{}
			copied = append(copied, value)
		}
		if len(copied) > 0 {
			result = append(result, copied)
		}
	}
	return result
}

func mergeStringGroups(dst, src [][]string) [][]string {
	result := cloneStringGroups(dst)
	for _, group := range cloneStringGroups(src) {
		duplicate := false
		for _, existing := range result {
			if sameStringSlice(existing, group) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			result = append(result, group)
		}
	}
	return result
}

func sameStringSlice(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
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
	u.ExtraUsageUnits = mergeExtraUsageUnitsMap(u.ExtraUsageUnits, other.ExtraUsageUnits)
	u.MergeExtraBilling(other.ExtraBilling)
	u.BillingDiagnostics = mergeBillingDiagnostics(u.BillingDiagnostics, other.BillingDiagnostics)
	u.ProviderExtraBilling = mergeBillingDiagnostics(u.ProviderExtraBilling, other.ProviderExtraBilling)
	u.ProviderIndependentUsageUnits = mergeBillingDiagnostics(u.ProviderIndependentUsageUnits, other.ProviderIndependentUsageUnits)
	u.ProviderTokenFields = mergeBillingDiagnostics(u.ProviderTokenFields, other.ProviderTokenFields)
	u.RequiredTokenExtraKeys = mergeStringSlice(u.RequiredTokenExtraKeys, other.RequiredTokenExtraKeys)
	u.TokenExtraEvidenceGroups = mergeStringGroups(u.TokenExtraEvidenceGroups, other.TokenExtraEvidenceGroups)
	u.ProviderTokenEvidence = u.ProviderTokenEvidence || other.ProviderTokenEvidence
	u.ProviderTokenConflict = u.ProviderTokenConflict || other.ProviderTokenConflict
	if other.ProviderOperationUnits != nil {
		if u.ProviderOperationUnits == nil {
			count := *other.ProviderOperationUnits
			u.ProviderOperationUnits = &count
		} else {
			*u.ProviderOperationUnits += *other.ProviderOperationUnits
		}
	}
	if other.ResponseModel != "" {
		if u.ResponseModel != "" && u.ResponseModel != other.ResponseModel {
			u.AttributionConflict = true
			u.BillingDiagnostics = mergeBillingDiagnostics(u.BillingDiagnostics, map[string]bool{"billing_model_conflict": true})
		} else {
			u.ResponseModel = other.ResponseModel
		}
	}
	if other.ServiceTier != "" {
		if u.ServiceTier != "" && u.ServiceTier != other.ServiceTier {
			u.AttributionConflict = true
			u.BillingDiagnostics = mergeBillingDiagnostics(u.BillingDiagnostics, map[string]bool{"billing_tier_conflict": true})
		} else {
			u.ServiceTier = other.ServiceTier
		}
	}
	speedEvidence := &Usage{Speed: u.Speed, SpeedConflict: u.SpeedConflict}
	speedEvidence.MergeProviderSpeed(other.Speed, other.SpeedConflict)
	u.Speed, u.SpeedConflict = speedEvidence.Speed, speedEvidence.SpeedConflict
	u.BillingDiagnostics = mergeBillingDiagnostics(u.BillingDiagnostics, speedEvidence.BillingDiagnostics)
	u.AttributionConflict = u.AttributionConflict || other.AttributionConflict
	if u.Source == "" && other.Source != "" {
		u.Source = other.Source
	}
}
