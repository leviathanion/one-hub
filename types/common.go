package types

import (
	"bytes"
	"encoding/json"
	"math"
	"net/http"
	"one-api/common/config"
	"strings"
)

type Usage struct {
	PromptTokens            int                     `json:"prompt_tokens"`
	CompletionTokens        int                     `json:"completion_tokens"`
	TotalTokens             int                     `json:"total_tokens"`
	PromptTokensDetails     PromptTokensDetails     `json:"prompt_tokens_details"`
	CompletionTokensDetails CompletionTokensDetails `json:"completion_tokens_details"`

	ExtraTokens            map[string]int          `json:"-"`
	ExtraUsageUnits        map[string]float64      `json:"-"`
	ExtraBilling           map[string]ExtraBilling `json:"-"`
	BillingDiagnostics     map[string]bool         `json:"-"`
	ResponseModel          string                  `json:"-"`
	ServiceTier            string                  `json:"-"`
	Speed                  string                  `json:"-"`
	SpeedConflict          bool                    `json:"-"`
	ProviderReported       bool                    `json:"-"`
	ProviderTokenFields    map[string]bool         `json:"-"`
	ProviderExtraBilling   map[string]bool         `json:"-"`
	RequiredTokenExtraKeys []string                `json:"-"`
	// TokenExtraEvidenceGroups records provider-declared mutually exclusive,
	// exhaustive token partitions.  It is billing metadata only: deriving a
	// missing remainder from a total never marks a provider field as observed.
	TokenExtraEvidenceGroups      [][]string             `json:"-"`
	AttributionConflict           bool                   `json:"-"`
	ProviderTokenConflict         bool                   `json:"-"`
	ProviderOperationUnits        *int                   `json:"-"`
	ProviderIndependentUsageUnits map[string]bool        `json:"-"`
	ProviderServerToolUse         *ProviderServerToolUse `json:"server_tool_use,omitempty"`
	ProviderPromptCacheHitTokens  int                    `json:"prompt_cache_hit_tokens,omitempty"`
	ProviderPromptCacheMissTokens int                    `json:"prompt_cache_miss_tokens,omitempty"`
	providerWireObserved          bool                   `json:"-"`
}

type ProviderServerToolUse struct {
	WebSearchRequests int `json:"web_search_requests"`
}

// UnmarshalJSON records wire presence but does not authorize billing. Only a
// provider-local extractor may call MarkProviderReported after it has verified
// correlation and the operation's evidence contract.
func (u *Usage) UnmarshalJSON(data []byte) error {
	type usageAlias Usage
	var decoded usageAlias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*u = Usage(decoded)
	u.providerWireObserved = true
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err == nil {
		u.ProviderTokenFields = make(map[string]bool)
		for _, field := range []string{"prompt_tokens", "completion_tokens", "total_tokens"} {
			if validUsageInteger(fields[field]) {
				u.ProviderTokenFields[field] = true
			}
		}
		markUsageDetailPresence(u.ProviderTokenFields, fields["prompt_tokens_details"], true)
		markUsageDetailPresence(u.ProviderTokenFields, fields["completion_tokens_details"], false)
		if validUsageInteger(fields["prompt_cache_hit_tokens"]) {
			u.ProviderTokenFields[config.UsageExtraDeepSeekCacheHit] = true
		}
		if validUsageInteger(fields["prompt_cache_miss_tokens"]) {
			u.ProviderTokenFields[config.UsageExtraDeepSeekCacheMiss] = true
		}
	}
	return nil
}

func markUsageDetailPresence(presence map[string]bool, raw json.RawMessage, prompt bool) {
	if len(raw) == 0 || presence == nil {
		return
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return
	}
	mapping := map[string]string{
		"audio_tokens":               config.UsageExtraOutputAudio,
		"text_tokens":                config.UsageExtraOutputTextTokens,
		"image_tokens":               config.UsageExtraOutputImageTokens,
		"reasoning_tokens":           config.UsageExtraReasoning,
		"accepted_prediction_tokens": "accepted_prediction_tokens",
		"rejected_prediction_tokens": "rejected_prediction_tokens",
	}
	if prompt {
		mapping = map[string]string{
			"audio_tokens":           config.UsageExtraInputAudio,
			"cached_tokens":          config.UsageExtraCache,
			"text_tokens":            config.UsageExtraInputTextTokens,
			"image_tokens":           config.UsageExtraInputImageTokens,
			"cached_tokens_internal": "cached_tokens_internal",
			"cache_write_tokens":     config.UsageExtraCacheWrite,
			"cached_write_tokens":    config.UsageExtraCachedWrite,
			"cached_read_tokens":     config.UsageExtraCachedRead,
		}
	}
	for field, key := range mapping {
		if validUsageInteger(fields[field]) {
			presence[key] = true
		}
	}
}

func validUsageInteger(raw json.RawMessage) bool {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return false
	}
	var value int
	return json.Unmarshal(raw, &value) == nil
}

func (u *Usage) MarkProviderReported() {
	if u != nil {
		u.ProviderReported = true
		if len(u.ProviderTokenFields) == 0 && !u.providerWireObserved {
			u.ProviderTokenFields = map[string]bool{"prompt_tokens": true, "completion_tokens": true, "total_tokens": true}
		}
		for key := range u.GetExtraTokens() {
			u.ProviderTokenFields[key] = true
		}
	}
}

// MarkProviderTokenField records a token field that the provider explicitly
// returned, including a valid zero.  Adapters must call this only for fields
// observed on the provider wire; derived remainders stay unmarked.
func (u *Usage) MarkProviderTokenField(key string) {
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

// MergeProviderAttribution keeps non-empty provider attribution sticky. Empty
// terminal fields cannot erase an earlier value, while disagreement makes all
// dependent price components conflicting.
func (u *Usage) MergeProviderAttribution(model, serviceTier string) {
	if u == nil {
		return
	}
	model = strings.TrimSpace(model)
	serviceTier = strings.TrimSpace(serviceTier)
	if model != "" {
		if u.ResponseModel != "" && u.ResponseModel != model {
			u.AttributionConflict = true
			if u.BillingDiagnostics == nil {
				u.BillingDiagnostics = make(map[string]bool)
			}
			u.BillingDiagnostics["billing_model_conflict"] = true
		} else {
			u.ResponseModel = model
		}
	}
	if serviceTier != "" {
		if u.ServiceTier != "" && u.ServiceTier != serviceTier {
			u.AttributionConflict = true
			if u.BillingDiagnostics == nil {
				u.BillingDiagnostics = make(map[string]bool)
			}
			u.BillingDiagnostics["billing_tier_conflict"] = true
		} else {
			u.ServiceTier = serviceTier
		}
	}
}

// MergeProviderSpeed 只接收适配器已归属的实际速度证据；冲突仅影响依赖速度的规则。
func (u *Usage) MergeProviderSpeed(speed string, conflict bool) {
	if u == nil {
		return
	}
	speed = strings.TrimSpace(speed)
	u.SpeedConflict = u.SpeedConflict || conflict || (speed != "" && u.Speed != "" && speed != u.Speed)
	if u.Speed == "" {
		u.Speed = speed
	}
	if u.SpeedConflict {
		u.MergeBillingDiagnostics(map[string]bool{"billing_speed_conflict": true})
	}
}

func (u *Usage) HasProviderUsage() bool {
	if !u.HasProviderBaseUsage() {
		return false
	}
	return u.HasRequiredTokenExtraEvidence()
}

// HasProviderBaseUsage reports whether the provider supplied the required,
// non-negative prompt/completion base token snapshot.  total_tokens is a
// redundant assertion for operations that expose it and is checked by that
// operation's adapter when present.  Optional token partitions are evaluated
// against the effective price by relay_util; they are deliberately excluded
// here so a missing optional detail cannot erase a valid base snapshot.
func (u *Usage) HasProviderBaseUsage() bool {
	if u == nil || !u.ProviderReported || u.PromptTokens < 0 || u.CompletionTokens < 0 || u.TotalTokens < 0 || u.AttributionConflict || u.ProviderTokenConflict {
		return false
	}
	if (u.providerWireObserved || len(u.ProviderTokenFields) > 0) && (!u.ProviderTokenFields["prompt_tokens"] || !u.ProviderTokenFields["completion_tokens"]) {
		return false
	}
	for _, value := range []int{
		u.PromptTokensDetails.AudioTokens,
		u.PromptTokensDetails.CachedTokens,
		u.PromptTokensDetails.TextTokens,
		u.PromptTokensDetails.ImageTokens,
		u.PromptTokensDetails.CachedTokensInternal,
		u.PromptTokensDetails.CacheWriteTokens,
		u.PromptTokensDetails.CachedWriteTokens,
		u.PromptTokensDetails.CachedReadTokens,
		u.CompletionTokensDetails.AudioTokens,
		u.CompletionTokensDetails.TextTokens,
		u.CompletionTokensDetails.ReasoningTokens,
		u.CompletionTokensDetails.AcceptedPredictionTokens,
		u.CompletionTokensDetails.RejectedPredictionTokens,
		u.CompletionTokensDetails.ImageTokens,
	} {
		if value < 0 {
			return false
		}
	}
	for _, value := range u.ExtraTokens {
		if value < 0 {
			return false
		}
	}
	for _, value := range u.ExtraUsageUnits {
		if value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
			return false
		}
	}
	for _, value := range u.ExtraBilling {
		if value.CallCount < 0 {
			return false
		}
	}
	return true
}

// HasRequiredTokenExtraEvidence retains the strict provider-declared evidence
// check for callers that need to expose only a fully partitioned snapshot.
func (u *Usage) HasRequiredTokenExtraEvidence() bool {
	if u == nil {
		return false
	}
	for _, key := range u.RequiredTokenExtraKeys {
		if key = strings.TrimSpace(key); key != "" && !u.ProviderTokenFields[key] {
			return false
		}
	}
	return true
}

func (u *Usage) MarkProviderOperationUnits(count int) {
	if u == nil || count < 0 {
		return
	}
	u.ProviderOperationUnits = new(int)
	*u.ProviderOperationUnits = count
}

func (u *Usage) SetProviderIndependentUsageUnit(key string, value float64) {
	key = strings.TrimSpace(key)
	if u == nil || key == "" || value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
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

func (u *Usage) RequireTokenExtraEvidence(keys ...string) {
	if u == nil || len(keys) == 0 {
		return
	}
	seen := make(map[string]struct{}, len(u.RequiredTokenExtraKeys)+len(keys))
	merged := make([]string, 0, len(u.RequiredTokenExtraKeys)+len(keys))
	for _, key := range append(append([]string(nil), u.RequiredTokenExtraKeys...), keys...) {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		merged = append(merged, key)
	}
	u.RequiredTokenExtraKeys = merged
}

// SetTokenExtraEvidenceGroups stores provider-declared partition contracts.
// The values are copied so later adapter mutations cannot alter the billing
// decision.  This method does not change RequiredTokenExtraKeys or field
// presence.
func (u *Usage) SetTokenExtraEvidenceGroups(groups ...[]string) {
	if u == nil {
		return
	}
	if len(groups) == 0 {
		u.TokenExtraEvidenceGroups = nil
		return
	}
	u.TokenExtraEvidenceGroups = make([][]string, 0, len(groups))
	for _, group := range groups {
		seen := make(map[string]struct{}, len(group))
		copied := make([]string, 0, len(group))
		for _, key := range group {
			key = strings.TrimSpace(key)
			if key == "" {
				continue
			}
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			copied = append(copied, key)
		}
		if len(copied) > 0 {
			u.TokenExtraEvidenceGroups = append(u.TokenExtraEvidenceGroups, copied)
		}
	}
}

func (u *Usage) MarkProviderExtraBilling(key string, billing ExtraBilling) {
	if u == nil {
		return
	}
	key = BuildExtraBillingKey(ResolveExtraBillingServiceType(key, billing), ResolveExtraBillingType(key, billing))
	if key == "" {
		return
	}
	if u.ProviderExtraBilling == nil {
		u.ProviderExtraBilling = make(map[string]bool)
	}
	u.ProviderExtraBilling[key] = true
}

func (u *Usage) IncProviderExtraBilling(serviceType, billingType string) {
	if u == nil {
		return
	}
	u.IncExtraBilling(serviceType, billingType)
	key := BuildExtraBillingKey(serviceType, billingType)
	if billing, ok := u.ExtraBilling[key]; ok {
		u.MarkProviderExtraBilling(key, billing)
	}
}

func (u *Usage) SetProviderExtraBilling(serviceType, billingType string, callCount int) {
	if u == nil || callCount < 0 {
		return
	}
	key := BuildExtraBillingKey(serviceType, billingType)
	if key == "" {
		return
	}
	if u.ExtraBilling == nil {
		u.ExtraBilling = make(map[string]ExtraBilling)
	}
	billing := ExtraBilling{ServiceType: serviceType, Type: billingType, CallCount: callCount}
	u.ExtraBilling[key] = billing
	u.MarkProviderExtraBilling(key, billing)
}

func (u *Usage) HasProviderExtraBilling(key string) bool {
	return u != nil && u.ProviderExtraBilling[strings.TrimSpace(key)]
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

func cloneExtraUsageUnitsMap(units map[string]float64) map[string]float64 {
	if len(units) == 0 {
		return nil
	}
	cloned := make(map[string]float64, len(units))
	for key, value := range units {
		cloned[key] = value
	}
	return cloned
}

func mergeExtraUsageUnitsMap(dst map[string]float64, src map[string]float64) map[string]float64 {
	if len(src) == 0 {
		return dst
	}
	if dst == nil {
		dst = make(map[string]float64, len(src))
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

func cloneBillingDiagnostics(diagnostics map[string]bool) map[string]bool {
	if len(diagnostics) == 0 {
		return nil
	}
	cloned := make(map[string]bool, len(diagnostics))
	for key, value := range diagnostics {
		cloned[key] = value
	}
	return cloned
}

func mergeBillingDiagnostics(dst map[string]bool, src map[string]bool) map[string]bool {
	if len(src) == 0 {
		return dst
	}
	if dst == nil {
		dst = make(map[string]bool, len(src))
	}
	for key, value := range src {
		if value && strings.TrimSpace(key) != "" {
			dst[key] = true
		}
	}
	return dst
}

func (u *Usage) AddBillingDiagnostic(diagnostic string) {
	if u == nil || strings.TrimSpace(diagnostic) == "" {
		return
	}
	if u.BillingDiagnostics == nil {
		u.BillingDiagnostics = make(map[string]bool)
	}
	u.BillingDiagnostics[diagnostic] = true
}

func (u *Usage) MergeBillingDiagnostics(diagnostics map[string]bool) {
	if u == nil {
		return
	}
	u.BillingDiagnostics = mergeBillingDiagnostics(u.BillingDiagnostics, diagnostics)
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
	fillMissingPositiveExtraToken(extraTokens, config.UsageExtraCachedRead, input.CachedReadTokens)
	fillMissingPositiveExtraToken(extraTokens, config.UsageExtraCacheWrite, input.CacheWriteTokens)
	fillMissingPositiveExtraToken(extraTokens, config.UsageExtraInputAudio, input.AudioTokens)
	fillMissingPositiveExtraToken(extraTokens, config.UsageExtraInputTextTokens, input.TextTokens)
	fillMissingPositiveExtraToken(extraTokens, config.UsageExtraCachedWrite, input.CachedWriteTokens)
	fillMissingPositiveExtraToken(extraTokens, config.UsageExtraInputImageTokens, input.ImageTokens)
	fillMissingPositiveExtraToken(extraTokens, config.UsageExtraOutputImageTokens, output.ImageTokens)
	fillMissingPositiveExtraToken(extraTokens, config.UsageExtraOutputAudio, output.AudioTokens)
	fillMissingPositiveExtraToken(extraTokens, config.UsageExtraOutputTextTokens, output.TextTokens)
	fillMissingPositiveExtraToken(extraTokens, config.UsageExtraReasoning, output.ReasoningTokens)
	return extraTokens
}

func fillMissingPositiveExtraToken(extraTokens map[string]int, key string, value int) {
	if _, present := extraTokens[key]; value > 0 && !present {
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

func (u *Usage) SetExtraUsageUnits(key string, value float64) {
	if u == nil || value <= 0 {
		return
	}
	if u.ExtraUsageUnits == nil {
		u.ExtraUsageUnits = make(map[string]float64)
	}
	u.ExtraUsageUnits[key] = value
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

	CacheWriteTokens  int `json:"cache_write_tokens,omitempty"`
	CachedWriteTokens int `json:"cached_write_tokens,omitempty"`
	CachedReadTokens  int `json:"cached_read_tokens,omitempty"`
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
	i.CacheWriteTokens += other.CacheWriteTokens
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
	StatusCode             int         `json:"status_code"`
	LocalError             bool        `json:"-"`
	UpstreamNotAttempted   bool        `json:"-"`
	UpstreamAccepted       bool        `json:"-"`
	UpstreamAmbiguous      bool        `json:"-"`
	ProviderOpenRetrySafe  bool        `json:"-"`
	ProviderQuotaExhausted bool        `json:"-"`
	ProviderAuthRejected   bool        `json:"-"`
	ProviderRateLimited    bool        `json:"-"`
	ReplayRawResponse      bool        `json:"-"`
	RawBody                []byte      `json:"-"`
	ResponseHeaders        http.Header `json:"-"`
}

type OpenAIErrorResponse struct {
	Error OpenAIError `json:"error,omitempty"`
}

type StreamOptions struct {
	IncludeUsage       bool  `json:"include_usage,omitempty"`
	IncludeObfuscation *bool `json:"include_obfuscation,omitempty"`
}

func (u *Usage) IncExtraBilling(key string, bType string) {
	u.ExtraBilling = incExtraBillingMap(u.ExtraBilling, key, bType)
}
