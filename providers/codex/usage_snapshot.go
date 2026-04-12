package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"one-api/common/cache"
	"one-api/common/logger"
	"one-api/common/requester"
)

const (
	usagePreviewCacheKeyPrefix = "codex:usage:preview"
	usageDetailCacheKeyPrefix  = "codex:usage:detail"
	// Trade-off: keep a short TTL so admin reads can reuse recent successful
	// snapshots, but TTL only bounds staleness. Correctness still depends on
	// explicit invalidation when the underlying channel changes or is deleted.
	usageCacheTTL         = time.Minute
	fiveHourWindowSeconds = 5 * 60 * 60
	weeklyWindowSeconds   = 7 * 24 * 60 * 60
)

type CodexUsageAccount struct {
	UserID    string `json:"user_id,omitempty"`
	Email     string `json:"email,omitempty"`
	AccountID string `json:"account_id,omitempty"`
}

type CodexUsageWindow struct {
	WindowKey       string   `json:"window_key"`
	Label           string   `json:"label"`
	Used            *float64 `json:"used,omitempty"`
	Limit           *float64 `json:"limit,omitempty"`
	Remaining       *float64 `json:"remaining,omitempty"`
	UsedPercent     *float64 `json:"used_percent,omitempty"`
	UsageRatio      *float64 `json:"usage_ratio,omitempty"`
	WindowSeconds   int64    `json:"window_seconds"`
	ResetsAt        int64    `json:"resets_at"`
	ResetsInSeconds int64    `json:"resets_in_seconds"`
}

type CodexUsagePreview struct {
	ChannelID    int                `json:"channel_id"`
	PlanType     string             `json:"plan_type,omitempty"`
	Allowed      *bool              `json:"allowed,omitempty"`
	LimitReached *bool              `json:"limit_reached,omitempty"`
	FetchedAt    int64              `json:"fetched_at"`
	Windows      []CodexUsageWindow `json:"windows"`
}

type CodexUsageSnapshot struct {
	ChannelID      int                `json:"channel_id"`
	Account        *CodexUsageAccount `json:"account,omitempty"`
	PlanType       string             `json:"plan_type,omitempty"`
	Allowed        *bool              `json:"allowed,omitempty"`
	LimitReached   *bool              `json:"limit_reached,omitempty"`
	UpstreamStatus int                `json:"upstream_status"`
	FetchedAt      int64              `json:"fetched_at"`
	Windows        []CodexUsageWindow `json:"windows"`
	Raw            any                `json:"raw,omitempty"`
}

type codexWhamUsageResponse struct {
	PlanType  any                `json:"plan_type,omitempty"`
	UserID    any                `json:"user_id,omitempty"`
	Email     any                `json:"email,omitempty"`
	AccountID any                `json:"account_id,omitempty"`
	RateLimit codexWhamRateLimit `json:"rate_limit,omitempty"`
}

type codexWhamRateLimit struct {
	PlanType        any              `json:"plan_type,omitempty"`
	Allowed         *bool            `json:"allowed,omitempty"`
	LimitReached    *bool            `json:"limit_reached,omitempty"`
	PrimaryWindow   *codexWhamWindow `json:"primary_window,omitempty"`
	SecondaryWindow *codexWhamWindow `json:"secondary_window,omitempty"`
}

type codexWhamWindow struct {
	UsedPercent        any `json:"used_percent,omitempty"`
	Used               any `json:"used,omitempty"`
	Limit              any `json:"limit,omitempty"`
	Remaining          any `json:"remaining,omitempty"`
	ResetAt            any `json:"reset_at,omitempty"`
	ResetsAt           any `json:"resets_at,omitempty"`
	ResetAfterSeconds  any `json:"reset_after_seconds,omitempty"`
	ResetsInSeconds    any `json:"resets_in_seconds,omitempty"`
	LimitWindowSeconds any `json:"limit_window_seconds,omitempty"`
	LimitWindowMinutes any `json:"limit_window_minutes,omitempty"`
}

type usageWindowCandidate struct {
	source string
	window CodexUsageWindow
}

func (p *CodexProvider) GetUsagePreview(ctx context.Context, forceRefresh bool) (*CodexUsagePreview, error) {
	if p == nil || p.Channel == nil {
		return nil, fmt.Errorf("provider not configured")
	}

	if !forceRefresh {
		if cachedPreview, err := getCachedUsagePreview(p.Channel.Id); err == nil {
			return &cachedPreview, nil
		}
	}

	snapshot, err := p.GetUsageSnapshot(ctx, forceRefresh)
	if snapshot == nil {
		return nil, err
	}

	preview := BuildUsagePreview(snapshot)
	if err == nil {
		cacheUsagePreview(preview)
	}
	return preview, err
}

func (p *CodexProvider) GetUsageSnapshot(ctx context.Context, forceRefresh bool) (*CodexUsageSnapshot, error) {
	if p == nil || p.Channel == nil {
		return nil, fmt.Errorf("provider not configured")
	}

	if !forceRefresh {
		if cachedSnapshot, err := getCachedUsageSnapshot(p.Channel.Id); err == nil {
			return &cachedSnapshot, nil
		}
	}

	snapshot, err := p.fetchUsageSnapshot(ctx)
	if snapshot != nil && err == nil {
		cacheSuccessfulUsageSnapshot(snapshot)
	}
	return snapshot, err
}

func BuildUsagePreview(snapshot *CodexUsageSnapshot) *CodexUsagePreview {
	if snapshot == nil {
		return nil
	}

	windows := make([]CodexUsageWindow, 0, len(snapshot.Windows))
	windows = append(windows, snapshot.Windows...)

	return &CodexUsagePreview{
		ChannelID:    snapshot.ChannelID,
		PlanType:     snapshot.PlanType,
		Allowed:      snapshot.Allowed,
		LimitReached: snapshot.LimitReached,
		FetchedAt:    snapshot.FetchedAt,
		Windows:      windows,
	}
}

func cacheSuccessfulUsageSnapshot(snapshot *CodexUsageSnapshot) {
	if snapshot == nil {
		return
	}

	cacheUsageSnapshot(snapshot)
	// Preview cache is only a projection of the latest successful usage snapshot.
	// Failed upstream fetches may still return a partial snapshot for diagnostics,
	// but caching that payload as a preview would hide the upstream failure and make
	// stale or incomplete data look valid. If we need to dampen repeated failures in
	// the future, add a dedicated short-lived error cache instead of reusing the
	// success preview cache.
	cacheUsagePreview(BuildUsagePreview(snapshot))
}

func (p *CodexProvider) fetchUsageSnapshot(ctx context.Context) (*CodexUsageSnapshot, error) {
	if p == nil || p.Channel == nil {
		return nil, fmt.Errorf("provider not configured")
	}

	snapshot, err := p.fetchUsageSnapshotOnce(ctx)
	if err == nil {
		return snapshot, nil
	}
	if snapshot == nil || snapshot.UpstreamStatus != http.StatusUnauthorized && snapshot.UpstreamStatus != http.StatusForbidden {
		return snapshot, err
	}
	if p.Credentials == nil || strings.TrimSpace(p.Credentials.RefreshToken) == "" {
		return snapshot, err
	}

	if _, refreshErr := p.forceRefreshToken(ctx); refreshErr != nil {
		if ctx != nil {
			logger.LogWarn(ctx, fmt.Sprintf("[Codex] forced refresh after usage fetch failure failed: %s", refreshErr.Error()))
		} else {
			logger.SysError("[Codex] forced refresh after usage fetch failure failed: " + refreshErr.Error())
		}
		return snapshot, err
	}

	refreshedSnapshot, retryErr := p.fetchUsageSnapshotOnce(ctx)
	if retryErr != nil {
		return refreshedSnapshot, retryErr
	}
	return refreshedSnapshot, nil
}

func (p *CodexProvider) fetchUsageSnapshotOnce(ctx context.Context) (*CodexUsageSnapshot, error) {
	headers, err := p.getUsageRequestHeaders()
	if err != nil {
		return nil, err
	}

	if p.Requester != nil {
		p.Requester.Context = ensureContext(ctx)
	}

	requestURL := p.GetFullRequestURL("/backend-api/wham/usage", "")
	req, err := p.Requester.NewRequest(http.MethodGet, requestURL, p.Requester.WithHeader(headers))
	if err != nil {
		return nil, err
	}

	resp, err := requester.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	snapshot, normalizeErr := normalizeUsageSnapshot(p.Channel.Id, p.Credentials, resp.StatusCode, bodyBytes)
	if normalizeErr != nil {
		return snapshot, normalizeErr
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return snapshot, errors.New(extractUsageErrorMessage(snapshot.Raw, resp.StatusCode))
	}

	return snapshot, nil
}

func (p *CodexProvider) getUsageRequestHeaders() (map[string]string, error) {
	headers, err := p.getRequestHeaderBag()
	if err != nil {
		return nil, err
	}

	headers.Set("Accept", "application/json")
	headers.SetIfAbsent("Originator", defaultOriginator)
	headers.SetIfAbsent("User-Agent", p.getLegacyUserAgentOverride())
	if strings.TrimSpace(headers.Get("User-Agent")) == "" {
		headers.Set("User-Agent", defaultUserAgent)
	}

	return headers.Map(), nil
}

func normalizeUsageSnapshot(channelID int, credentials *OAuth2Credentials, statusCode int, bodyBytes []byte) (*CodexUsageSnapshot, error) {
	snapshot := &CodexUsageSnapshot{
		ChannelID:      channelID,
		UpstreamStatus: statusCode,
		FetchedAt:      time.Now().Unix(),
		Windows:        make([]CodexUsageWindow, 0, 2),
	}

	if len(strings.TrimSpace(string(bodyBytes))) == 0 {
		snapshot.Account = normalizeUsageAccount(nil, credentials)
		return snapshot, fmt.Errorf("empty Codex usage payload")
	}

	var raw any
	if err := json.Unmarshal(bodyBytes, &raw); err != nil {
		snapshot.Raw = string(bodyBytes)
		snapshot.Account = normalizeUsageAccount(nil, credentials)
		return snapshot, fmt.Errorf("failed to decode Codex usage payload: %w", err)
	}
	snapshot.Raw = raw

	var payload codexWhamUsageResponse
	if err := json.Unmarshal(bodyBytes, &payload); err != nil {
		snapshot.Account = normalizeUsageAccount(nil, credentials)
		return snapshot, fmt.Errorf("failed to normalize Codex usage payload: %w", err)
	}

	snapshot.PlanType = firstNonEmptyString(payload.PlanType, payload.RateLimit.PlanType)
	snapshot.Allowed = payload.RateLimit.Allowed
	snapshot.LimitReached = payload.RateLimit.LimitReached
	snapshot.Account = normalizeUsageAccount(&payload, credentials)
	snapshot.Windows = normalizeUsageWindows(&payload)

	return snapshot, nil
}

func normalizeUsageAccount(payload *codexWhamUsageResponse, credentials *OAuth2Credentials) *CodexUsageAccount {
	account := &CodexUsageAccount{}
	if payload != nil {
		account.UserID = stringifyJSONValue(payload.UserID)
		account.Email = stringifyJSONValue(payload.Email)
		account.AccountID = stringifyJSONValue(payload.AccountID)
	}
	if account.AccountID == "" && credentials != nil {
		account.AccountID = strings.TrimSpace(credentials.AccountID)
	}
	if account.UserID == "" && account.Email == "" && account.AccountID == "" {
		return nil
	}
	return account
}

func normalizeUsageWindows(payload *codexWhamUsageResponse) []CodexUsageWindow {
	if payload == nil {
		return []CodexUsageWindow{}
	}

	candidates := make([]usageWindowCandidate, 0, 2)
	if candidate := newUsageWindowCandidate("primary", payload.RateLimit.PrimaryWindow); candidate != nil {
		candidates = append(candidates, *candidate)
	}
	if candidate := newUsageWindowCandidate("secondary", payload.RateLimit.SecondaryWindow); candidate != nil {
		candidates = append(candidates, *candidate)
	}
	if len(candidates) == 0 {
		return []CodexUsageWindow{}
	}

	windows := make([]CodexUsageWindow, 0, len(candidates))
	seenWindowKeys := make(map[string]bool, 2)
	for _, candidate := range candidates {
		window := candidate.window
		switch classifyUsageWindow(window.WindowSeconds) {
		case "five_hour":
			if !seenWindowKeys["five_hour"] {
				window.WindowKey = "five_hour"
				window.Label = "5h"
				seenWindowKeys["five_hour"] = true
			}
		case "weekly":
			if !seenWindowKeys["weekly"] {
				window.WindowKey = "weekly"
				window.Label = "7d"
				seenWindowKeys["weekly"] = true
			}
		}
		if window.WindowKey == "" {
			window.WindowKey = "custom"
			window.Label = strings.Title(candidate.source)
		}
		windows = append(windows, window)
	}

	return windows
}

func newUsageWindowCandidate(source string, data *codexWhamWindow) *usageWindowCandidate {
	if data == nil {
		return nil
	}

	windowSeconds := firstNonZeroInt64(data.LimitWindowSeconds, data.LimitWindowMinutes)
	if windowSeconds > 0 && parseInt64(data.LimitWindowMinutes) == windowSeconds {
		windowSeconds *= 60
	}

	usedPercent := parseOptionalFloat64(data.UsedPercent)
	used := parseOptionalFloat64(data.Used)
	limit := parseOptionalFloat64(data.Limit)
	remaining := parseOptionalFloat64(data.Remaining)
	normalizedUsedPercent, usageRatio := normalizeUsageMetrics(usedPercent, used, limit)

	if limit != nil && *limit > 0 {
		if remaining == nil && used != nil {
			computedRemaining := *limit - *used
			if computedRemaining < 0 {
				computedRemaining = 0
			}
			remaining = &computedRemaining
		}
	}

	resetAfterSeconds := firstNonZeroInt64(data.ResetAfterSeconds, data.ResetsInSeconds)
	resetAt := firstNonZeroInt64(data.ResetAt, data.ResetsAt)
	if resetAt == 0 && resetAfterSeconds != 0 {
		resetAt = time.Now().Unix() + resetAfterSeconds
	}

	return &usageWindowCandidate{
		source: source,
		window: CodexUsageWindow{
			Used:            used,
			Limit:           limit,
			Remaining:       remaining,
			UsedPercent:     normalizedUsedPercent,
			UsageRatio:      usageRatio,
			WindowSeconds:   windowSeconds,
			ResetsAt:        resetAt,
			ResetsInSeconds: resetAfterSeconds,
		},
	}
}

func normalizeUsageMetrics(explicitUsedPercent, used, limit *float64) (*float64, *float64) {
	// Correctness invariant: unknown usage stays absent instead of collapsing to
	// 0. An explicit 0 means upstream (or derived used/limit math) told us usage
	// is known and zero; nil means we do not have enough signal to claim that.
	if limit != nil && *limit > 0 && used != nil {
		derivedUsedPercentValue := (*used / *limit) * 100
		derivedUsageRatioValue := clampUsageRatio(*used / *limit)
		if explicitUsedPercent != nil {
			return explicitUsedPercent, &derivedUsageRatioValue
		}
		return &derivedUsedPercentValue, &derivedUsageRatioValue
	}
	if explicitUsedPercent != nil {
		derivedUsageRatioValue := clampUsageRatio(*explicitUsedPercent / 100)
		return explicitUsedPercent, &derivedUsageRatioValue
	}
	return nil, nil
}

func classifyUsageWindow(windowSeconds int64) string {
	// We only label windows when upstream exposes the canonical duration.
	// Trade-off: unknown durations render as custom/blank in the list instead of
	// guessing and showing a wrong 5h/7d badge.
	switch windowSeconds {
	case fiveHourWindowSeconds:
		return "five_hour"
	case weeklyWindowSeconds:
		return "weekly"
	default:
		return ""
	}
}

func extractUsageErrorMessage(raw any, statusCode int) string {
	if rawMap, ok := raw.(map[string]any); ok {
		if errorMap, ok := rawMap["error"].(map[string]any); ok {
			if message := stringifyJSONValue(errorMap["message"]); message != "" {
				return message
			}
		}
		if message := stringifyJSONValue(rawMap["message"]); message != "" {
			return message
		}
	}
	return fmt.Sprintf("upstream status %d", statusCode)
}

func parseOptionalFloat64(value any) *float64 {
	parsed := parseFloat64(value)
	if !hasMeaningfulNumber(value) {
		return nil
	}
	return &parsed
}

func parseFloat64(value any) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case float32:
		return float64(typed)
	case int:
		return float64(typed)
	case int64:
		return float64(typed)
	case int32:
		return float64(typed)
	case json.Number:
		parsed, _ := typed.Float64()
		return parsed
	case string:
		var parsed json.Number = json.Number(strings.TrimSpace(typed))
		floatValue, err := parsed.Float64()
		if err == nil {
			return floatValue
		}
	}
	return 0
}

func parseInt64(value any) int64 {
	switch typed := value.(type) {
	case int64:
		return typed
	case int:
		return int64(typed)
	case int32:
		return int64(typed)
	case float64:
		return int64(typed)
	case float32:
		return int64(typed)
	case json.Number:
		parsed, _ := typed.Int64()
		return parsed
	case string:
		var parsed json.Number = json.Number(strings.TrimSpace(typed))
		intValue, err := parsed.Int64()
		if err == nil {
			return intValue
		}
		floatValue, err := parsed.Float64()
		if err == nil {
			return int64(floatValue)
		}
	}
	return 0
}

func firstNonZeroInt64(values ...any) int64 {
	for _, value := range values {
		parsed := parseInt64(value)
		if parsed != 0 {
			return parsed
		}
	}
	return 0
}

func firstNonEmptyString(values ...any) string {
	for _, value := range values {
		if parsed := stringifyJSONValue(value); parsed != "" {
			return parsed
		}
	}
	return ""
}

func stringifyJSONValue(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(typed)
	case json.Number:
		return typed.String()
	default:
		text := strings.TrimSpace(fmt.Sprintf("%v", typed))
		if text == "<nil>" {
			return ""
		}
		return text
	}
}

func hasMeaningfulNumber(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(typed) != ""
	default:
		return true
	}
}

func clampUsageRatio(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 1 {
		return 1
	}
	return value
}

func getCachedUsagePreview(channelID int) (CodexUsagePreview, error) {
	return cache.GetCache[CodexUsagePreview](usagePreviewCacheKey(channelID))
}

func getCachedUsageSnapshot(channelID int) (CodexUsageSnapshot, error) {
	return cache.GetCache[CodexUsageSnapshot](usageDetailCacheKey(channelID))
}

func cacheUsagePreview(preview *CodexUsagePreview) {
	if preview == nil || preview.ChannelID <= 0 {
		return
	}
	if err := cache.SetCache(usagePreviewCacheKey(preview.ChannelID), preview, usageCacheTTL); err != nil {
		logger.SysError(fmt.Sprintf("[Codex] failed to cache usage preview for channel %d: %v", preview.ChannelID, err))
	}
}

func cacheUsageSnapshot(snapshot *CodexUsageSnapshot) {
	if snapshot == nil || snapshot.ChannelID <= 0 {
		return
	}
	if err := cache.SetCache(usageDetailCacheKey(snapshot.ChannelID), snapshot, usageCacheTTL); err != nil {
		logger.SysError(fmt.Sprintf("[Codex] failed to cache usage snapshot for channel %d: %v", snapshot.ChannelID, err))
	}
}

func usagePreviewCacheKey(channelID int) string {
	return fmt.Sprintf("%s:%d", usagePreviewCacheKeyPrefix, channelID)
}

func usageDetailCacheKey(channelID int) string {
	return fmt.Sprintf("%s:%d", usageDetailCacheKeyPrefix, channelID)
}
