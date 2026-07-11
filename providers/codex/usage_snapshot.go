package codex

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"one-api/common/cache"
	"one-api/common/logger"
	"one-api/common/requester"
	"one-api/model"

	"github.com/google/uuid"
)

var (
	errCodexUsageResponseTooLarge = errors.New("Codex usage upstream payload too large")
	// ErrResetCreditCommittedResponseUnusable means the irreversible reset POST
	// received a successful HTTP status, but its response could not be consumed.
	// Callers must not retry the business operation.
	ErrResetCreditCommittedResponseUnusable = errors.New("Codex reset credit accepted but response unusable")
)

const (
	usagePreviewCacheKeyPrefix = "codex:usage:v2:preview"
	usageDetailCacheKeyPrefix  = "codex:usage:v2:detail"
	usageResponseBodyMaxBytes  = 1 << 20
	usageGenerationMarkerTTL   = cache.CodexUsageGenerationTTL
	// Detail is an interactive view and stays short-lived. Preview has a separate
	// TTL owned by the background refresh policy in usage_auto_refresh.go.
	usageDetailCacheTTL   = time.Minute
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

type CodexRateLimitResetCredits struct {
	AvailableCount int `json:"available_count"`
}

type CodexUsageSnapshot struct {
	ChannelID             int                         `json:"channel_id"`
	Account               *CodexUsageAccount          `json:"account,omitempty"`
	PlanType              string                      `json:"plan_type,omitempty"`
	Allowed               *bool                       `json:"allowed,omitempty"`
	LimitReached          *bool                       `json:"limit_reached,omitempty"`
	UpstreamStatus        int                         `json:"upstream_status"`
	FetchedAt             int64                       `json:"fetched_at"`
	Windows               []CodexUsageWindow          `json:"windows"`
	RateLimitResetCredits *CodexRateLimitResetCredits `json:"rate_limit_reset_credits,omitempty"`
	Raw                   any                         `json:"raw,omitempty"`
}

type CodexResetCredit struct {
	ID         string `json:"id,omitempty"`
	Status     string `json:"status,omitempty"`
	RedeemedAt string `json:"redeemed_at,omitempty"`
}

type CodexResetResult struct {
	ChannelID      int               `json:"channel_id"`
	Code           string            `json:"code,omitempty"`
	WindowsReset   int               `json:"windows_reset"`
	Credit         *CodexResetCredit `json:"credit,omitempty"`
	upstreamStatus int
}

func IsResetCreditCommittedResponseUnusable(err error) bool {
	return errors.Is(err, ErrResetCreditCommittedResponseUnusable)
}

type codexWhamUsageResponse struct {
	PlanType              any                         `json:"plan_type,omitempty"`
	UserID                any                         `json:"user_id,omitempty"`
	Email                 any                         `json:"email,omitempty"`
	AccountID             any                         `json:"account_id,omitempty"`
	RateLimit             codexWhamRateLimit          `json:"rate_limit,omitempty"`
	RateLimitResetCredits *CodexRateLimitResetCredits `json:"rate_limit_reset_credits,omitempty"`
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

type codexResetCreditConsumeRequest struct {
	RedeemRequestID string `json:"redeem_request_id"`
}

type codexResetCreditConsumeResponse struct {
	Code         string            `json:"code,omitempty"`
	WindowsReset int               `json:"windows_reset,omitempty"`
	Credit       *CodexResetCredit `json:"credit,omitempty"`
	ResetCredit  *CodexResetCredit `json:"rate_limit_reset_credit,omitempty"`
	ID           string            `json:"id,omitempty"`
	Status       string            `json:"status,omitempty"`
	RedeemedAt   string            `json:"redeemed_at,omitempty"`
}

// Usage-cache boundary:
//
// Usage preview/detail is a disposable presentation projection, never part of
// OAuth credential recovery. A cache hit deliberately returns without calling
// commitPendingCredentials. Recovery liveness belongs to the scheduled
// ReconcilePendingCredentials pass, while every operation that actually consumes
// a token synchronously reconciles under the per-channel refresh lock.
//
// Trade-off: the UI may show a TTL-bounded stale usage projection while a rotated
// credential is waiting for database persistence. This keeps read-only usage
// views available during DB trouble without risking the credential: the pending
// journal has no TTL and cannot be removed by cache activity. Do not couple these
// paths merely to make a cache hit perform maintenance.
func (p *CodexProvider) GetUsagePreview(ctx context.Context, forceRefresh bool) (*CodexUsagePreview, error) {
	channel := p.codexChannel()
	if channel == nil {
		return nil, fmt.Errorf("provider not configured")
	}
	generation, generationErr := usageCacheGeneration(ctx, channel.Id)
	cacheAvailable := generationErr == nil
	if generationErr != nil {
		logger.SysError(fmt.Sprintf("[Codex] bypassing usage cache for channel %d: %v", channel.Id, generationErr))
	}

	if cacheAvailable && !forceRefresh {
		if cachedPreview, cacheErr := getCachedUsagePreviewForChannelGenerationContext(ctx, channel, generation); cacheErr == nil {
			return &cachedPreview, nil
		} else if !errors.Is(cacheErr, cache.CacheNotFound) {
			cacheAvailable = false
			logger.SysError(fmt.Sprintf("[Codex] bypassing usage cache for channel %d after read failure: %v", channel.Id, cacheErr))
		}
	}

	var snapshot *CodexUsageSnapshot
	var err error
	previewWriteAttempted := false
	if cacheAvailable {
		snapshot, previewWriteAttempted, err = p.getUsageSnapshotForGeneration(ctx, forceRefresh, false, generation)
	} else {
		snapshot, err = p.fetchUsageSnapshot(ctx)
	}
	if snapshot == nil {
		return nil, err
	}

	preview := BuildUsagePreview(snapshot)
	if cacheAvailable && err == nil && !previewWriteAttempted {
		if cacheErr := cacheUsagePreviewForGeneration(ctx, p.codexChannel(), preview, generation, usagePreviewCacheTTL); cacheErr != nil {
			logger.SysError(fmt.Sprintf("[Codex] failed to cache usage preview for channel %d: %v", channel.Id, cacheErr))
		}
	}
	return preview, err
}

func (p *CodexProvider) GetUsageSnapshot(ctx context.Context, forceRefresh bool, includeRaw ...bool) (*CodexUsageSnapshot, error) {
	channel := p.codexChannel()
	if channel == nil {
		return nil, fmt.Errorf("provider not configured")
	}
	generation, generationErr := usageCacheGeneration(ctx, channel.Id)
	if generationErr != nil {
		logger.SysError(fmt.Sprintf("[Codex] bypassing usage cache for channel %d: %v", channel.Id, generationErr))
		snapshot, err := p.fetchUsageSnapshot(ctx)
		if snapshot != nil && !(len(includeRaw) > 0 && includeRaw[0]) {
			snapshot = cloneCodexUsageSnapshot(snapshot, false)
		}
		return snapshot, err
	}
	snapshot, _, err := p.getUsageSnapshotForGeneration(ctx, forceRefresh, len(includeRaw) > 0 && includeRaw[0], generation)
	return snapshot, err
}

func (p *CodexProvider) getUsageSnapshotForGeneration(ctx context.Context, forceRefresh, rawRequested bool, generation string) (*CodexUsageSnapshot, bool, error) {
	channel := p.codexChannel()
	cacheAvailable := true
	if !forceRefresh && !rawRequested {
		if cachedSnapshot, cacheErr := getCachedUsageSnapshotForChannelGenerationContext(ctx, channel, generation); cacheErr == nil {
			return cloneCodexUsageSnapshot(&cachedSnapshot, false), false, nil
		} else if !errors.Is(cacheErr, cache.CacheNotFound) {
			cacheAvailable = false
			logger.SysError(fmt.Sprintf("[Codex] bypassing usage detail cache for channel %d after read failure: %v", channel.Id, cacheErr))
		}
	}

	snapshot, err := p.fetchUsageSnapshot(ctx)
	previewWriteAttempted := false
	if cacheAvailable && snapshot != nil && err == nil {
		// This helper warms detail and preview together. Report the attempted preview
		// write so GetUsagePreview does not write it a second time after a real fetch.
		previewWriteAttempted = true
		if cacheErr := cacheSuccessfulUsageSnapshotForGeneration(ctx, p.codexChannel(), cloneCodexUsageSnapshot(snapshot, false), generation); cacheErr != nil {
			logger.SysError(fmt.Sprintf("[Codex] failed to cache successful usage for channel %d: %v", channel.Id, cacheErr))
		}
	}
	if snapshot != nil && !rawRequested {
		snapshot = cloneCodexUsageSnapshot(snapshot, false)
	}
	return snapshot, previewWriteAttempted, err
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

func cloneCodexUsageSnapshot(snapshot *CodexUsageSnapshot, includeRaw bool) *CodexUsageSnapshot {
	if snapshot == nil {
		return nil
	}
	cloned := *snapshot
	if snapshot.Account != nil {
		account := *snapshot.Account
		cloned.Account = &account
	}
	cloned.Windows = append([]CodexUsageWindow(nil), snapshot.Windows...)
	if snapshot.RateLimitResetCredits != nil {
		credits := *snapshot.RateLimitResetCredits
		cloned.RateLimitResetCredits = &credits
	}
	if !includeRaw {
		cloned.Raw = nil
	}
	return &cloned
}

func (p *CodexProvider) ConsumeResetCredit(ctx context.Context) (*CodexResetResult, error) {
	if p == nil || p.codexChannel() == nil {
		return nil, fmt.Errorf("provider not configured")
	}

	result, err := p.consumeResetCreditOnce(ctx)
	if err == nil {
		return result, nil
	}
	if result == nil || result.upstreamStatus != http.StatusUnauthorized && result.upstreamStatus != http.StatusForbidden {
		return result, err
	}
	if p.Credentials == nil || strings.TrimSpace(p.Credentials.RefreshToken) == "" {
		return result, err
	}

	if _, refreshErr := p.forceRefreshToken(ctx); refreshErr != nil {
		if ctx != nil {
			logger.LogWarn(ctx, fmt.Sprintf("[Codex] forced refresh after reset credit failure failed: %s", refreshErr.Error()))
		} else {
			logger.SysError("[Codex] forced refresh after reset credit failure failed: " + refreshErr.Error())
		}
		return result, fmt.Errorf("forced refresh after reset credit failure: %w", refreshErr)
	}

	return p.consumeResetCreditOnce(ctx)
}

func (p *CodexProvider) consumeResetCreditOnce(ctx context.Context) (*CodexResetResult, error) {
	channel := p.codexChannel()
	if channel == nil {
		return nil, fmt.Errorf("provider not configured")
	}
	headers, err := p.getUsageRequestHeaders(ctx)
	if err != nil {
		return nil, err
	}

	httpRequester := p.codexRequester()
	if httpRequester == nil {
		return nil, fmt.Errorf("requester is not configured")
	}

	requestURL := p.GetFullRequestURL("/backend-api/wham/rate-limit-reset-credits/consume", "")
	req, err := httpRequester.NewRequest(
		http.MethodPost,
		requestURL,
		httpRequester.WithHeader(headers),
		httpRequester.WithBody(codexResetCreditConsumeRequest{RedeemRequestID: uuid.NewString()}),
		httpRequester.WithContext(ensureContext(ctx)),
	)
	if err != nil {
		return nil, err
	}
	if requester.HTTPClient == nil {
		return nil, fmt.Errorf("HTTP client is not configured")
	}

	resp, err := requester.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	result := newMinimalResetCreditResult(channel.Id, resp.StatusCode)
	bodyBytes, err := readCodexUsageResponseBody(resp.Body)
	if err != nil {
		if isHTTPSuccess(resp.StatusCode) {
			return result, fmt.Errorf("%w: %w", ErrResetCreditCommittedResponseUnusable, err)
		}
		return result, err
	}

	result, normalizeErr := normalizeResetCreditResultWithCredentials(channel.Id, p.Credentials, resp.StatusCode, bodyBytes)
	if normalizeErr != nil {
		if isHTTPSuccess(resp.StatusCode) {
			return result, fmt.Errorf("%w: %w", ErrResetCreditCommittedResponseUnusable, normalizeErr)
		}
		return result, normalizeErr
	}
	if !isHTTPSuccess(resp.StatusCode) {
		message := extractUsageErrorMessage(redactUsageCredentialValues(decodeRawJSON(bodyBytes), p.Credentials), resp.StatusCode)
		return result, errors.New(redactUsageCredentialSecrets(message, p.Credentials))
	}

	return result, nil
}

func cacheSuccessfulUsageSnapshot(ctx context.Context, channel *model.Channel, snapshot *CodexUsageSnapshot) error {
	generation, err := usageCacheGeneration(ctx, channel.Id)
	if err != nil {
		return err
	}
	return cacheSuccessfulUsageSnapshotForGeneration(ctx, channel, snapshot, generation)
}

func cacheSuccessfulUsageSnapshotForGeneration(ctx context.Context, channel *model.Channel, snapshot *CodexUsageSnapshot, generation string) error {
	if snapshot == nil {
		return nil
	}

	// Detail remains short-lived, while preview stays available across the periodic
	// refresh interval. Failed upstream payloads never reach this helper.
	return errors.Join(
		cacheUsageSnapshotForGeneration(ctx, channel, snapshot, generation, usageDetailCacheTTL),
		cacheUsagePreviewForGeneration(ctx, channel, BuildUsagePreview(snapshot), generation, usagePreviewCacheTTL),
	)
}

func (p *CodexProvider) fetchUsageSnapshot(ctx context.Context) (*CodexUsageSnapshot, error) {
	if p == nil || p.codexChannel() == nil {
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
		return snapshot, fmt.Errorf("forced refresh after usage fetch failure: %w", refreshErr)
	}

	refreshedSnapshot, retryErr := p.fetchUsageSnapshotOnce(ctx)
	if retryErr != nil {
		return refreshedSnapshot, retryErr
	}
	return refreshedSnapshot, nil
}

func (p *CodexProvider) fetchUsageSnapshotOnce(ctx context.Context) (*CodexUsageSnapshot, error) {
	channel := p.codexChannel()
	if channel == nil {
		return nil, fmt.Errorf("provider not configured")
	}
	headers, err := p.getUsageRequestHeaders(ctx)
	if err != nil {
		return nil, err
	}

	httpRequester := p.codexRequester()
	if httpRequester == nil {
		return nil, fmt.Errorf("requester is not configured")
	}
	if requester.HTTPClient == nil {
		return nil, fmt.Errorf("HTTP client is not configured")
	}

	requestURL := p.GetFullRequestURL("/backend-api/wham/usage", "")
	req, err := httpRequester.NewRequest(http.MethodGet, requestURL, httpRequester.WithHeader(headers), httpRequester.WithContext(ensureContext(ctx)))
	if err != nil {
		return nil, err
	}

	resp, err := requester.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	bodyBytes, err := readCodexUsageResponseBody(resp.Body)
	if err != nil {
		return nil, err
	}

	snapshot, normalizeErr := normalizeUsageSnapshot(channel.Id, p.Credentials, resp.StatusCode, bodyBytes)
	if normalizeErr != nil {
		return snapshot, normalizeErr
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		message := extractUsageErrorMessage(snapshot.Raw, resp.StatusCode)
		return snapshot, errors.New(redactUsageCredentialSecrets(message, p.Credentials))
	}

	return snapshot, nil
}

func (p *CodexProvider) getUsageRequestHeaders(ctx context.Context) (map[string]string, error) {
	headers, err := p.getRequestHeaderBagWithContext(ctx)
	if err != nil {
		return nil, err
	}

	headers.Set("Accept", "application/json")
	if strings.TrimSpace(headers.Get("User-Agent")) == "" {
		headers.Set("User-Agent", defaultUserAgent)
	}
	headers.SetIfAbsent("originator", resolveSmartOriginatorForEffectiveUserAgent(headers.Get("User-Agent")))

	return headers.Map(), nil
}

func readCodexUsageResponseBody(body io.Reader) ([]byte, error) {
	bodyBytes, err := io.ReadAll(io.LimitReader(body, usageResponseBodyMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(bodyBytes) > usageResponseBodyMaxBytes {
		return nil, fmt.Errorf("%w (limit %d bytes)", errCodexUsageResponseTooLarge, usageResponseBodyMaxBytes)
	}
	return bodyBytes, nil
}

func normalizeUsageSnapshot(channelID int, credentials *OAuth2Credentials, statusCode int, bodyBytes []byte) (*CodexUsageSnapshot, error) {
	snapshot := &CodexUsageSnapshot{
		ChannelID:      channelID,
		UpstreamStatus: statusCode,
		FetchedAt:      time.Now().Unix(),
		Windows:        make([]CodexUsageWindow, 0, 2),
	}

	if len(strings.TrimSpace(string(bodyBytes))) == 0 {
		snapshot.Raw = invalidCodexRawPlaceholder
		snapshot.Account = normalizeUsageAccount(nil, credentials)
		return snapshot, fmt.Errorf("empty Codex usage payload")
	}

	var raw any
	if !utf8.Valid(bodyBytes) {
		snapshot.Raw = invalidCodexRawPlaceholder
		snapshot.Account = normalizeUsageAccount(nil, credentials)
		return snapshot, fmt.Errorf("failed to decode Codex usage payload: invalid UTF-8")
	}
	if err := json.Unmarshal(bodyBytes, &raw); err != nil {
		snapshot.Raw = invalidCodexRawPlaceholder
		snapshot.Account = normalizeUsageAccount(nil, credentials)
		return snapshot, fmt.Errorf("failed to decode Codex usage payload: %w", err)
	}
	// Raw is admin-visible on request and can also survive normalize failures.
	// Sanitize at assignment so every subsequent return path is safe.
	snapshot.Raw = redactUsageCredentialValues(raw, credentials)

	var payload codexWhamUsageResponse
	if err := json.Unmarshal(bodyBytes, &payload); err != nil {
		snapshot.Account = normalizeUsageAccount(nil, credentials)
		return snapshot, fmt.Errorf("failed to normalize Codex usage payload: %w", err)
	}

	snapshot.PlanType = firstNonEmptyString(payload.PlanType, payload.RateLimit.PlanType)
	snapshot.Account = normalizeUsageAccount(&payload, credentials)
	snapshot.Windows = normalizeUsageWindows(&payload)
	snapshot.Allowed, snapshot.LimitReached = normalizeUsageLimitState(payload.RateLimit.Allowed, payload.RateLimit.LimitReached, snapshot.Windows)
	snapshot.RateLimitResetCredits = payload.RateLimitResetCredits

	return snapshot, nil
}

func normalizeResetCreditResult(channelID int, statusCode int, bodyBytes []byte) (*CodexResetResult, error) {
	return normalizeResetCreditResultWithCredentials(channelID, nil, statusCode, bodyBytes)
}

func normalizeResetCreditResultWithCredentials(channelID int, _ *OAuth2Credentials, statusCode int, bodyBytes []byte) (*CodexResetResult, error) {
	result := newMinimalResetCreditResult(channelID, statusCode)

	if len(strings.TrimSpace(string(bodyBytes))) == 0 {
		return result, fmt.Errorf("empty Codex reset credit payload")
	}
	if !utf8.Valid(bodyBytes) {
		return result, fmt.Errorf("failed to decode Codex reset credit payload: invalid UTF-8")
	}

	var payload codexResetCreditConsumeResponse
	if err := json.Unmarshal(bodyBytes, &payload); err != nil {
		return result, fmt.Errorf("failed to normalize Codex reset credit payload: %w", err)
	}

	result.Code = firstNonEmptyString(payload.Code, result.Code)
	result.WindowsReset = payload.WindowsReset
	if isHTTPSuccess(statusCode) && result.WindowsReset == 0 {
		result.WindowsReset = 1
	}
	if payload.Credit != nil {
		result.Credit = payload.Credit
	} else if payload.ResetCredit != nil {
		result.Credit = payload.ResetCredit
	} else if payload.ID != "" || payload.Status != "" || payload.RedeemedAt != "" {
		result.Credit = &CodexResetCredit{
			ID:         payload.ID,
			Status:     payload.Status,
			RedeemedAt: payload.RedeemedAt,
		}
	}

	return result, nil
}

func newMinimalResetCreditResult(channelID, statusCode int) *CodexResetResult {
	return &CodexResetResult{
		ChannelID:      channelID,
		Code:           fmt.Sprintf("%d", statusCode),
		upstreamStatus: statusCode,
	}
}

func isHTTPSuccess(statusCode int) bool {
	return statusCode >= http.StatusOK && statusCode < http.StatusMultipleChoices
}

const invalidCodexRawPlaceholder = "[omitted: invalid JSON]"

func decodeRawJSON(bodyBytes []byte) any {
	if !utf8.Valid(bodyBytes) {
		return invalidCodexRawPlaceholder
	}
	var raw any
	if err := json.Unmarshal(bodyBytes, &raw); err != nil {
		return invalidCodexRawPlaceholder
	}
	return raw
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
		switch classifyUsageWindow(candidate.source, window.WindowSeconds) {
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

func normalizeUsageLimitState(upstreamAllowed *bool, upstreamLimitReached *bool, windows []CodexUsageWindow) (*bool, *bool) {
	limitReached := upstreamLimitReached
	if limitReached == nil {
		if inferredLimitReached, ok := inferLimitReachedFromUsageWindows(windows); ok {
			limitReached = boolPtr(inferredLimitReached)
		}
	}

	allowed := upstreamAllowed
	if allowed == nil && limitReached != nil {
		allowed = boolPtr(!*limitReached)
	}
	if limitReached == nil && allowed != nil {
		limitReached = boolPtr(!*allowed)
	}

	return allowed, limitReached
}

func inferLimitReachedFromUsageWindows(windows []CodexUsageWindow) (bool, bool) {
	if len(windows) == 0 {
		return false, false
	}

	known := false
	for _, window := range windows {
		reached, ok := usageWindowLimitReached(window)
		if !ok {
			continue
		}
		known = true
		if reached {
			return true, true
		}
	}
	if known {
		return false, true
	}
	return false, false
}

func usageWindowLimitReached(window CodexUsageWindow) (bool, bool) {
	if window.Limit != nil && *window.Limit > 0 {
		if window.Used != nil {
			return *window.Used >= *window.Limit, true
		}
		if window.Remaining != nil {
			return *window.Remaining <= 0, true
		}
	}
	if window.UsageRatio != nil {
		return *window.UsageRatio >= 1, true
	}
	if window.UsedPercent != nil {
		return *window.UsedPercent >= 100, true
	}
	return false, false
}

func boolPtr(value bool) *bool {
	return &value
}

func classifyUsageWindow(source string, windowSeconds int64) string {
	// Prefer explicit upstream duration. Some Codex payloads omit one duration
	// when only one limiter is active, so we fall back to the legacy header role
	// used by CLIProxyAPI/sub2api only when duration is absent: primary is the
	// weekly window and secondary is the 5h window.
	switch windowSeconds {
	case fiveHourWindowSeconds:
		return "five_hour"
	case weeklyWindowSeconds:
		return "weekly"
	case 0:
		switch strings.ToLower(strings.TrimSpace(source)) {
		case "primary":
			return "weekly"
		case "secondary":
			return "five_hour"
		}
	}
	return ""
}

func redactUsageCredentialSecrets(text string, credentials *OAuth2Credentials) string {
	clientID := ""
	if credentials != nil {
		clientID = credentials.ClientID
	}
	return redactTokenRefreshSecrets(text, credentials, clientID)
}

func isSensitiveCredentialField(key string) bool {
	// JSON producers vary between snake_case, kebab-case, camelCase and
	// PascalCase. Compare a separator-free alphanumeric form so all equivalent
	// spellings receive whole-value redaction.
	normalized := strings.Map(func(r rune) rune {
		if !unicode.IsLetter(r) && !unicode.IsNumber(r) {
			return -1
		}
		return unicode.ToLower(r)
	}, key)
	switch normalized {
	case "accesstoken", "refreshtoken", "idtoken", "authorization", "clientsecret", "clientassertion", "apikey", "xapikey", "token":
		return true
	default:
		return false
	}
}

func redactUsageCredentialValues(value any, credentials *OAuth2Credentials) any {
	switch typed := value.(type) {
	case string:
		return redactUsageCredentialSecrets(typed, credentials)
	case []any:
		for i := range typed {
			typed[i] = redactUsageCredentialValues(typed[i], credentials)
		}
		return typed
	case map[string]any:
		redacted := make(map[string]any, len(typed))
		for key, item := range typed {
			redactedKey := redactUsageCredentialSecrets(key, credentials)
			if isSensitiveCredentialField(key) {
				redacted[redactedKey] = "[redacted]"
			} else {
				redacted[redactedKey] = redactUsageCredentialValues(item, credentials)
			}
		}
		return redacted
	default:
		return value
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

type codexUsageCacheEntry[T any] struct {
	Fingerprint string `msgpack:"fingerprint"`
	Value       T      `msgpack:"value"`
}

var setCodexUsageCache = func(ctx context.Context, key string, value any, ttl time.Duration) error {
	return cache.SetCacheContext(ctx, key, value, ttl)
}

func getCachedUsagePreviewForChannel(channel *model.Channel, operationCtx ...context.Context) (CodexUsagePreview, error) {
	ctx := usageOperationContext(operationCtx)
	generation, err := usageCacheGeneration(ctx, channel.Id)
	if err != nil {
		return CodexUsagePreview{}, err
	}
	return getCachedUsagePreviewForChannelGenerationContext(ctx, channel, generation)
}

func getCachedUsagePreviewForChannelGeneration(channel *model.Channel, generation string) (CodexUsagePreview, error) {
	return getCachedUsagePreviewForChannelGenerationContext(context.Background(), channel, generation)
}

func getCachedUsagePreviewForChannelGenerationContext(ctx context.Context, channel *model.Channel, generation string) (CodexUsagePreview, error) {
	fingerprint := codexUsageChannelFingerprint(channel)
	if fingerprint == "" {
		return CodexUsagePreview{}, cache.CacheNotFound
	}
	entry, err := cache.GetCacheContext[codexUsageCacheEntry[CodexUsagePreview]](ctx, usagePreviewCacheKey(channel.Id, generation, fingerprint))
	if err != nil {
		return CodexUsagePreview{}, err
	}
	if entry.Fingerprint != fingerprint {
		return CodexUsagePreview{}, cache.CacheNotFound
	}
	return entry.Value, nil
}

func getCachedUsageSnapshotForChannel(channel *model.Channel, operationCtx ...context.Context) (CodexUsageSnapshot, error) {
	ctx := usageOperationContext(operationCtx)
	generation, err := usageCacheGeneration(ctx, channel.Id)
	if err != nil {
		return CodexUsageSnapshot{}, err
	}
	return getCachedUsageSnapshotForChannelGenerationContext(ctx, channel, generation)
}

func getCachedUsageSnapshotForChannelGeneration(channel *model.Channel, generation string) (CodexUsageSnapshot, error) {
	return getCachedUsageSnapshotForChannelGenerationContext(context.Background(), channel, generation)
}

func getCachedUsageSnapshotForChannelGenerationContext(ctx context.Context, channel *model.Channel, generation string) (CodexUsageSnapshot, error) {
	fingerprint := codexUsageChannelFingerprint(channel)
	if fingerprint == "" {
		return CodexUsageSnapshot{}, cache.CacheNotFound
	}
	entry, err := cache.GetCacheContext[codexUsageCacheEntry[CodexUsageSnapshot]](ctx, usageDetailCacheKey(channel.Id, generation, fingerprint))
	if err != nil {
		return CodexUsageSnapshot{}, err
	}
	if entry.Fingerprint != fingerprint {
		return CodexUsageSnapshot{}, cache.CacheNotFound
	}
	return entry.Value, nil
}

func cacheUsagePreview(channel *model.Channel, preview *CodexUsagePreview, operationCtx ...context.Context) error {
	return cacheUsagePreviewWithTTL(channel, preview, usagePreviewCacheTTL, operationCtx...)
}

func cacheUsagePreviewWithTTL(channel *model.Channel, preview *CodexUsagePreview, ttl time.Duration, operationCtx ...context.Context) error {
	generation, err := usageCacheGeneration(usageOperationContext(operationCtx), channel.Id)
	if err != nil {
		return err
	}
	return cacheUsagePreviewForGeneration(usageOperationContext(operationCtx), channel, preview, generation, ttl)
}

func cacheUsagePreviewForGeneration(ctx context.Context, channel *model.Channel, preview *CodexUsagePreview, generation string, ttl time.Duration) error {
	if preview == nil {
		return nil
	}
	if channel == nil || channel.Id <= 0 || preview.ChannelID != channel.Id {
		return fmt.Errorf("usage preview channel does not match cache channel")
	}
	if ttl <= 0 {
		ttl = usagePreviewCacheTTL
	}
	fingerprint := codexUsageChannelFingerprint(channel)
	entry := codexUsageCacheEntry[CodexUsagePreview]{
		Fingerprint: fingerprint,
		Value:       *preview,
	}
	if err := setCodexUsageCache(ctx, usagePreviewCacheKey(preview.ChannelID, generation, fingerprint), entry, ttl); err != nil {
		return fmt.Errorf("cache usage preview for channel %d: %w", preview.ChannelID, err)
	}
	return nil
}

func cacheUsageSnapshotWithTTL(channel *model.Channel, snapshot *CodexUsageSnapshot, ttl time.Duration, operationCtx ...context.Context) error {
	generation, err := usageCacheGeneration(usageOperationContext(operationCtx), channel.Id)
	if err != nil {
		return err
	}
	return cacheUsageSnapshotForGeneration(usageOperationContext(operationCtx), channel, snapshot, generation, ttl)
}

func cacheUsageSnapshotForGeneration(ctx context.Context, channel *model.Channel, snapshot *CodexUsageSnapshot, generation string, ttl time.Duration) error {
	if snapshot == nil {
		return nil
	}
	if channel == nil || channel.Id <= 0 || snapshot.ChannelID != channel.Id {
		return fmt.Errorf("usage snapshot channel does not match cache channel")
	}
	if ttl <= 0 {
		ttl = usageDetailCacheTTL
	}
	fingerprint := codexUsageChannelFingerprint(channel)
	entry := codexUsageCacheEntry[CodexUsageSnapshot]{
		Fingerprint: fingerprint,
		Value:       *cloneCodexUsageSnapshot(snapshot, false),
	}
	if err := setCodexUsageCache(ctx, usageDetailCacheKey(snapshot.ChannelID, generation, fingerprint), entry, ttl); err != nil {
		return fmt.Errorf("cache usage snapshot for channel %d: %w", snapshot.ChannelID, err)
	}
	return nil
}

func codexUsageChannelFingerprint(channel *model.Channel) string {
	if channel == nil || channel.Id <= 0 {
		return ""
	}
	// Key isolates credential rotations; CreatedTime distinguishes a deleted and
	// recreated row that reused the same numeric ID and configuration. Only the
	// digest becomes part of the cache key, never the raw credential.
	payload := struct {
		ID           int    `json:"id"`
		CreatedTime  int64  `json:"created_time"`
		Type         int    `json:"type"`
		Status       int    `json:"status"`
		Key          string `json:"key"`
		BaseURL      string `json:"base_url"`
		Proxy        string `json:"proxy"`
		Other        string `json:"other"`
		ModelHeaders string `json:"model_headers"`
	}{
		ID:           channel.Id,
		CreatedTime:  channel.CreatedTime,
		Type:         channel.Type,
		Status:       channel.Status,
		Key:          channel.Key,
		BaseURL:      usageCachePointerValue(channel.BaseURL),
		Proxy:        usageCachePointerValue(channel.Proxy),
		Other:        channel.Other,
		ModelHeaders: usageCachePointerValue(channel.ModelHeaders),
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(encoded)
	return fmt.Sprintf("%x", digest[:])
}

func usageCachePointerValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func usageOperationContext(contexts []context.Context) context.Context {
	if len(contexts) > 0 && contexts[0] != nil {
		return contexts[0]
	}
	return context.Background()
}

var usageCacheGeneration = func(ctx context.Context, channelID int) (string, error) {
	return cache.GetOrInitCodexUsageGenerationContext(ctx, channelID)
}

func RotateUsageCacheGeneration(channelID int) error {
	return cache.RotateCodexUsageGeneration(channelID)
}

// ClearUsageCacheForChannel is retained for callers that need invalidation; a
// generation rotation invalidates every credential fingerprint, including writes
// still in flight on another instance.
func ClearUsageCacheForChannel(channel *model.Channel) error {
	if channel == nil {
		return nil
	}
	return RotateUsageCacheGeneration(channel.Id)
}

func usageGenerationKey(channelID int) string {
	return fmt.Sprintf("%s:%d", cache.CodexUsageGenerationKeyPrefix, channelID)
}

func usagePreviewCacheKey(channelID int, generation, fingerprint string) string {
	return fmt.Sprintf("%s:%d:%s:%s", usagePreviewCacheKeyPrefix, channelID, generation, fingerprint)
}

func usageDetailCacheKey(channelID int, generation, fingerprint string) string {
	return fmt.Sprintf("%s:%d:%s:%s", usageDetailCacheKeyPrefix, channelID, generation, fingerprint)
}
