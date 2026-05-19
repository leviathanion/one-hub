package types

import (
	"encoding/json"
	"one-api/common/config"
	"strings"
)

type Usage struct {
	PromptTokens            int                     `json:"prompt_tokens"`
	CompletionTokens        int                     `json:"completion_tokens"`
	TotalTokens             int                     `json:"total_tokens"`
	PromptTokensDetails     PromptTokensDetails     `json:"prompt_tokens_details"`
	CompletionTokensDetails CompletionTokensDetails `json:"completion_tokens_details"`

	ExtraTokens  map[string]int          `json:"-"`
	ExtraBilling map[string]ExtraBilling `json:"-"`
	TextBuilder  strings.Builder         `json:"-"`
}

type ExtraBilling struct {
	ServiceType string `json:"service_type,omitempty"`
	Type        string `json:"type"`
	CallCount   int    `json:"call_count"`
}

const extraBillingVariantSeparator = "|"

func cloneExtraTokensMap(extraTokens map[string]int) map[string]int {
	if len(extraTokens) == 0 {
		return nil
	}

	cloned := make(map[string]int, len(extraTokens))
	for key, value := range extraTokens {
		cloned[key] = value
	}
	return cloned
}

func mergeExtraTokensMap(dst map[string]int, src map[string]int) map[string]int {
	if len(src) == 0 {
		return dst
	}
	if dst == nil {
		dst = make(map[string]int, len(src))
	}
	for key, value := range src {
		dst[key] += value
	}
	return dst
}

func cloneExtraBillingMap(extraBilling map[string]ExtraBilling) map[string]ExtraBilling {
	if len(extraBilling) == 0 {
		return nil
	}

	cloned := make(map[string]ExtraBilling, len(extraBilling))
	for key, value := range extraBilling {
		cloned[key] = value
	}
	return cloned
}

func BuildExtraBillingKey(serviceType, bType string) string {
	serviceType = strings.TrimSpace(serviceType)
	bType = strings.TrimSpace(bType)
	if serviceType == "" {
		return ""
	}
	if !extraBillingVariantKeyed(serviceType) || bType == "" {
		return serviceType
	}
	return serviceType + extraBillingVariantSeparator + bType
}

func ResolveExtraBillingServiceType(key string, billing ExtraBilling) string {
	if serviceType := strings.TrimSpace(billing.ServiceType); serviceType != "" {
		return serviceType
	}
	serviceType, _, _ := strings.Cut(strings.TrimSpace(key), extraBillingVariantSeparator)
	return strings.TrimSpace(serviceType)
}

func ResolveExtraBillingType(key string, billing ExtraBilling) string {
	if bType := strings.TrimSpace(billing.Type); bType != "" {
		return bType
	}
	_, bType, ok := strings.Cut(strings.TrimSpace(key), extraBillingVariantSeparator)
	if !ok {
		return ""
	}
	return strings.TrimSpace(bType)
}

func extraBillingVariantKeyed(serviceType string) bool {
	switch strings.TrimSpace(serviceType) {
	case APIToolTypeImageGeneration:
		return true
	default:
		return false
	}
}

func fillExtraTokensFromDetails(extraTokens map[string]int, input PromptTokensDetails, output CompletionTokensDetails) map[string]int {
	if extraTokens == nil {
		extraTokens = make(map[string]int)
	}

	// Adapter contract: callers that emit incremental usage must populate
	// ExtraTokens with the delta first. Detail fields are treated as snapshots
	// and only fill missing keys; this preserves explicit adapter values.
	fillMissingPositiveExtraToken(extraTokens, config.UsageExtraCache, input.CachedTokens)
	fillMissingPositiveExtraToken(extraTokens, config.UsageExtraInputAudio, input.AudioTokens)
	fillMissingPositiveExtraToken(extraTokens, config.UsageExtraInputTextTokens, input.TextTokens)
	fillMissingPositiveExtraToken(extraTokens, config.UsageExtraCachedWrite, input.CachedWriteTokens)
	fillMissingPositiveExtraToken(extraTokens, config.UsageExtraCachedRead, input.CachedReadTokens)
	fillMissingPositiveExtraToken(extraTokens, config.UsageExtraInputImageTokens, input.ImageTokens)
	fillMissingPositiveExtraToken(extraTokens, config.UsageExtraOutputImageTokens, output.ImageTokens)
	fillMissingPositiveExtraToken(extraTokens, config.UsageExtraOutputAudio, output.AudioTokens)
	fillMissingPositiveExtraToken(extraTokens, config.UsageExtraOutputTextTokens, output.TextTokens)
	fillMissingPositiveExtraToken(extraTokens, config.UsageExtraReasoning, output.ReasoningTokens)
	return extraTokens
}

func fillMissingPositiveExtraToken(extraTokens map[string]int, key string, value int) {
	if value > 0 && extraTokens[key] == 0 {
		extraTokens[key] = value
	}
}

func mergeExtraBillingMap(dst map[string]ExtraBilling, extraBilling map[string]ExtraBilling) map[string]ExtraBilling {
	if len(extraBilling) == 0 {
		return dst
	}
	if dst == nil {
		dst = make(map[string]ExtraBilling, len(extraBilling))
	}
	for key, value := range extraBilling {
		serviceType := ResolveExtraBillingServiceType(key, value)
		bType := ResolveExtraBillingType(key, value)
		key = BuildExtraBillingKey(serviceType, bType)
		if key == "" {
			continue
		}
		billing := dst[key]
		if billing.ServiceType == "" {
			billing.ServiceType = serviceType
		}
		if billing.Type == "" {
			billing.Type = bType
		}
		billing.CallCount += value.CallCount
		dst[key] = billing
	}
	return dst
}

func incExtraBillingMap(dst map[string]ExtraBilling, key string, bType string) map[string]ExtraBilling {
	key = BuildExtraBillingKey(key, bType)
	if key == "" {
		return dst
	}
	if dst == nil {
		dst = make(map[string]ExtraBilling)
	}

	billing := dst[key]
	if billing.ServiceType == "" {
		billing.ServiceType = ResolveExtraBillingServiceType(key, billing)
	}
	if billing.Type == "" {
		billing.Type = ResolveExtraBillingType(key, ExtraBilling{Type: bType})
	}
	billing.CallCount++
	dst[key] = billing
	return dst
}

func (u *Usage) GetExtraTokens() map[string]int {
	u.ExtraTokens = fillExtraTokensFromDetails(u.ExtraTokens, u.PromptTokensDetails, u.CompletionTokensDetails)
	return u.ExtraTokens
}

func (u *Usage) SetExtraTokens(key string, value int) {
	if u.ExtraTokens == nil {
		u.ExtraTokens = make(map[string]int)
	}

	u.ExtraTokens[key] = value
}

func (u *Usage) MergeExtraBilling(extraBilling map[string]ExtraBilling) {
	u.ExtraBilling = mergeExtraBillingMap(u.ExtraBilling, extraBilling)
}

type PromptTokensDetails struct {
	AudioTokens          int `json:"audio_tokens,omitempty"`
	CachedTokens         int `json:"cached_tokens,omitempty"`
	TextTokens           int `json:"text_tokens,omitempty"`
	ImageTokens          int `json:"image_tokens,omitempty"`
	CachedTokensInternal int `json:"cached_tokens_internal,omitempty"`

	CachedWriteTokens int `json:"-"`
	CachedReadTokens  int `json:"-"`
}

type CompletionTokensDetails struct {
	AudioTokens              int `json:"audio_tokens,omitempty"`
	TextTokens               int `json:"text_tokens,omitempty"`
	ReasoningTokens          int `json:"reasoning_tokens"`
	AcceptedPredictionTokens int `json:"accepted_prediction_tokens"`
	RejectedPredictionTokens int `json:"rejected_prediction_tokens"`
	ImageTokens              int `json:"image_tokens,omitempty"`
}

func (i *PromptTokensDetails) Merge(other *PromptTokensDetails) {
	if other == nil {
		return
	}

	i.AudioTokens += other.AudioTokens
	i.CachedTokens += other.CachedTokens
	i.TextTokens += other.TextTokens
	i.ImageTokens += other.ImageTokens
	i.CachedTokensInternal += other.CachedTokensInternal
	i.CachedWriteTokens += other.CachedWriteTokens
	i.CachedReadTokens += other.CachedReadTokens
}

func (o *CompletionTokensDetails) Merge(other *CompletionTokensDetails) {
	if other == nil {
		return
	}

	o.AudioTokens += other.AudioTokens
	o.TextTokens += other.TextTokens
	o.ReasoningTokens += other.ReasoningTokens
	o.AcceptedPredictionTokens += other.AcceptedPredictionTokens
	o.RejectedPredictionTokens += other.RejectedPredictionTokens
	o.ImageTokens += other.ImageTokens
}

type OpenAIError struct {
	Code       any    `json:"code,omitempty"`
	Message    string `json:"message"`
	Param      string `json:"param,omitempty"`
	Type       string `json:"type,omitempty"`
	InnerError any    `json:"innererror,omitempty"`
}

func (e *OpenAIError) Error() string {
	response := &OpenAIErrorResponse{
		Error: *e,
	}

	// 转换为JSON
	bytes, _ := json.Marshal(response)

	return string(bytes)
}

type OpenAIErrorWithStatusCode struct {
	OpenAIError
	StatusCode int  `json:"status_code"`
	LocalError bool `json:"-"`
}

type OpenAIErrorResponse struct {
	Error OpenAIError `json:"error,omitempty"`
}

type StreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

func (u *Usage) IncExtraBilling(key string, bType string) {
	u.ExtraBilling = incExtraBillingMap(u.ExtraBilling, key, bType)
}
