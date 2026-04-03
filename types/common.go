package types

import (
	"encoding/json"
	"fmt"
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

func (u *Usage) GetExtraTokens() map[string]int {
	if u.ExtraTokens == nil {
		u.ExtraTokens = make(map[string]int)
	}

	// 组装，已有的数据

	// 缓存数据
	if u.PromptTokensDetails.CachedTokens > 0 && u.ExtraTokens[config.UsageExtraCache] == 0 {
		u.ExtraTokens[config.UsageExtraCache] = u.PromptTokensDetails.CachedTokens
	}

	// 输入音频
	if u.PromptTokensDetails.AudioTokens > 0 && u.ExtraTokens[config.UsageExtraInputAudio] == 0 {
		u.ExtraTokens[config.UsageExtraInputAudio] = u.PromptTokensDetails.AudioTokens
	}

	// 输入文字
	if u.PromptTokensDetails.TextTokens > 0 && u.ExtraTokens[config.UsageExtraInputTextTokens] == 0 {
		u.ExtraTokens[config.UsageExtraInputTextTokens] = u.PromptTokensDetails.TextTokens
	}

	// 缓存写入
	if u.PromptTokensDetails.CachedWriteTokens > 0 && u.ExtraTokens[config.UsageExtraCachedWrite] == 0 {
		u.ExtraTokens[config.UsageExtraCachedWrite] = u.PromptTokensDetails.CachedWriteTokens
	}

	// 缓存读取
	if u.PromptTokensDetails.CachedReadTokens > 0 && u.ExtraTokens[config.UsageExtraCachedRead] == 0 {
		u.ExtraTokens[config.UsageExtraCachedRead] = u.PromptTokensDetails.CachedReadTokens
	}

	// 输入图像
	if u.PromptTokensDetails.ImageTokens > 0 && u.ExtraTokens[config.UsageExtraInputImageTokens] == 0 {
		u.ExtraTokens[config.UsageExtraInputImageTokens] = u.PromptTokensDetails.ImageTokens
	}

	// 输出图像
	if u.CompletionTokensDetails.ImageTokens > 0 && u.ExtraTokens[config.UsageExtraOutputImageTokens] == 0 {
		u.ExtraTokens[config.UsageExtraOutputImageTokens] = u.CompletionTokensDetails.ImageTokens
	}

	// 输出音频
	if u.CompletionTokensDetails.AudioTokens > 0 && u.ExtraTokens[config.UsageExtraOutputAudio] == 0 {
		u.ExtraTokens[config.UsageExtraOutputAudio] = u.CompletionTokensDetails.AudioTokens
	}

	// 输出文字
	if u.CompletionTokensDetails.TextTokens > 0 && u.ExtraTokens[config.UsageExtraOutputTextTokens] == 0 {
		u.ExtraTokens[config.UsageExtraOutputTextTokens] = u.CompletionTokensDetails.TextTokens
	}

	// 推理
	if u.CompletionTokensDetails.ReasoningTokens > 0 && u.ExtraTokens[config.UsageExtraReasoning] == 0 {
		u.ExtraTokens[config.UsageExtraReasoning] = u.CompletionTokensDetails.ReasoningTokens
	}

	return u.ExtraTokens
}

func (u *Usage) SetExtraTokens(key string, value int) {
	if u.ExtraTokens == nil {
		u.ExtraTokens = make(map[string]int)
	}

	u.ExtraTokens[key] = value
}

func (u *Usage) MergeExtraBilling(extraBilling map[string]ExtraBilling) {
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
}

func (o *CompletionTokensDetails) Merge(other *CompletionTokensDetails) {
	if other == nil {
		return
	}

	o.AudioTokens += other.AudioTokens
	o.TextTokens += other.TextTokens
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

	fmt.Println("e", string(bytes))
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
