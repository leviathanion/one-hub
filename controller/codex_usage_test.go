package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"one-api/common/cache"
	"one-api/common/config"
	commonTest "one-api/common/test"
	"one-api/model"
	"one-api/providers/codex"

	"github.com/gin-gonic/gin"
)

type fakeCodexUsageProvider struct {
	preview              *codex.CodexUsagePreview
	previewErr           error
	snapshot             *codex.CodexUsageSnapshot
	snapshotErr          error
	resetResult          *codex.CodexResetResult
	resetErr             error
	previewForceRefresh  bool
	snapshotForceRefresh bool
	snapshotIncludeRaw   bool
	resetCalled          bool
}

func (f *fakeCodexUsageProvider) GetUsagePreview(_ context.Context, forceRefresh bool) (*codex.CodexUsagePreview, error) {
	f.previewForceRefresh = forceRefresh
	return f.preview, f.previewErr
}

func (f *fakeCodexUsageProvider) GetUsageSnapshot(_ context.Context, forceRefresh bool, includeRaw ...bool) (*codex.CodexUsageSnapshot, error) {
	f.snapshotForceRefresh = forceRefresh
	f.snapshotIncludeRaw = len(includeRaw) > 0 && includeRaw[0]
	return f.snapshot, f.snapshotErr
}

func (f *fakeCodexUsageProvider) ConsumeResetCredit(_ context.Context) (*codex.CodexResetResult, error) {
	f.resetCalled = true
	return f.resetResult, f.resetErr
}

func TestValidateCodexUsageChannelAllowsTaggedCodexChannels(t *testing.T) {
	channel := &model.Channel{
		Id:   7,
		Type: config.ChannelTypeCodex,
		Tag:  "codex-team",
		Key:  "credentials",
	}

	if err := validateCodexUsageChannel(channel); err != nil {
		t.Fatalf("expected tagged Codex channel to be valid, got %v", err)
	}
}

func TestGetCodexChannelUsageAllowsTaggedChannels(t *testing.T) {
	gin.SetMode(gin.TestMode)

	originalLoadByID := loadCodexUsageChannelByID
	originalCreateProvider := createCodexUsageProvider
	t.Cleanup(func() {
		loadCodexUsageChannelByID = originalLoadByID
		createCodexUsageProvider = originalCreateProvider
	})

	provider := &fakeCodexUsageProvider{
		snapshot: &codex.CodexUsageSnapshot{
			ChannelID: 7,
			PlanType:  "pro",
		},
	}

	loadCodexUsageChannelByID = func(channelID int) (*model.Channel, error) {
		return &model.Channel{
			Id:   channelID,
			Type: config.ChannelTypeCodex,
			Tag:  "codex-team",
			Key:  "credentials",
		}, nil
	}
	createCodexUsageProvider = func(channel *model.Channel) (codexUsageProvider, error) {
		if channel.Tag != "codex-team" {
			t.Fatalf("expected tagged channel to reach provider creation, got tag %q", channel.Tag)
		}
		return provider, nil
	}

	ctx, recorder := commonTest.GetContext(http.MethodGet, "/api/channel/7/codex/usage?refresh=1", commonTest.RequestJSONConfig(), nil)
	ctx.Params = gin.Params{{Key: "id", Value: "7"}}

	GetCodexChannelUsage(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", recorder.Code)
	}

	var resp struct {
		Success bool                      `json:"success"`
		Message string                    `json:"message"`
		Data    *codex.CodexUsageSnapshot `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if !resp.Success {
		t.Fatalf("expected success response, got body %s", recorder.Body.String())
	}
	if resp.Data == nil || resp.Data.ChannelID != 7 {
		t.Fatalf("expected tagged channel snapshot, got %+v", resp.Data)
	}
	if !provider.snapshotForceRefresh {
		t.Fatalf("expected detail endpoint to force refresh the usage snapshot")
	}
	if provider.snapshotIncludeRaw {
		t.Fatalf("expected detail endpoint not to request raw payload by default")
	}
}

func TestGetCodexChannelUsageDebugRawRequestsRawSnapshot(t *testing.T) {
	gin.SetMode(gin.TestMode)

	originalLoadByID := loadCodexUsageChannelByID
	originalCreateProvider := createCodexUsageProvider
	t.Cleanup(func() {
		loadCodexUsageChannelByID = originalLoadByID
		createCodexUsageProvider = originalCreateProvider
	})

	provider := &fakeCodexUsageProvider{
		snapshot: &codex.CodexUsageSnapshot{
			ChannelID: 7,
			PlanType:  "pro",
			Raw:       map[string]any{"email": "user@example.com"},
		},
	}

	loadCodexUsageChannelByID = func(channelID int) (*model.Channel, error) {
		return &model.Channel{Id: channelID, Type: config.ChannelTypeCodex, Key: "credentials"}, nil
	}
	createCodexUsageProvider = func(channel *model.Channel) (codexUsageProvider, error) {
		return provider, nil
	}

	ctx, recorder := commonTest.GetContext(http.MethodGet, "/api/channel/7/codex/usage?debug_raw=1", commonTest.RequestJSONConfig(), nil)
	ctx.Params = gin.Params{{Key: "id", Value: "7"}}

	GetCodexChannelUsage(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", recorder.Code)
	}
	if !provider.snapshotIncludeRaw {
		t.Fatal("expected debug_raw=1 to request raw payload")
	}
	if !strings.Contains(recorder.Body.String(), "user@example.com") {
		t.Fatalf("expected debug response to include raw payload, got %s", recorder.Body.String())
	}
}

func TestConsumeCodexResetCreditAllowsTaggedChannelsAndClearsCache(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cache.InitCacheManager()

	originalLoadByID := loadCodexUsageChannelByID
	originalCreateProvider := createCodexUsageProvider
	t.Cleanup(func() {
		loadCodexUsageChannelByID = originalLoadByID
		createCodexUsageProvider = originalCreateProvider
	})

	channelID := 7
	if err := cache.SetCache("codex:usage:preview:7", "cached-preview", time.Minute); err != nil {
		t.Fatalf("failed to seed preview cache: %v", err)
	}
	if err := cache.SetCache("codex:usage:detail:7", "cached-detail", time.Minute); err != nil {
		t.Fatalf("failed to seed detail cache: %v", err)
	}

	provider := &fakeCodexUsageProvider{
		resetResult: &codex.CodexResetResult{
			ChannelID:    channelID,
			Code:         "ok",
			WindowsReset: 1,
			Credit:       &codex.CodexResetCredit{ID: "credit-1", Status: "redeemed"},
		},
	}

	loadCodexUsageChannelByID = func(channelID int) (*model.Channel, error) {
		return &model.Channel{
			Id:   channelID,
			Type: config.ChannelTypeCodex,
			Tag:  "codex-team",
			Key:  "credentials",
		}, nil
	}
	createCodexUsageProvider = func(channel *model.Channel) (codexUsageProvider, error) {
		if channel.Tag != "codex-team" {
			t.Fatalf("expected tagged channel to reach provider creation, got tag %q", channel.Tag)
		}
		return provider, nil
	}

	ctx, recorder := commonTest.GetContext(http.MethodPost, "/api/channel/7/codex/reset-credit", commonTest.RequestJSONConfig(), nil)
	ctx.Params = gin.Params{{Key: "id", Value: "7"}}

	ConsumeCodexResetCredit(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", recorder.Code)
	}
	if !provider.resetCalled {
		t.Fatal("expected reset provider method to be called")
	}

	var resp struct {
		Success bool                    `json:"success"`
		Message string                  `json:"message"`
		Data    *codex.CodexResetResult `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if !resp.Success {
		t.Fatalf("expected success response, got body %s", recorder.Body.String())
	}
	if resp.Data == nil || resp.Data.ChannelID != channelID || resp.Data.WindowsReset != 1 {
		t.Fatalf("expected reset result payload, got %+v", resp.Data)
	}
	if _, err := cache.GetCache[string]("codex:usage:preview:7"); !errors.Is(err, cache.CacheNotFound) {
		t.Fatalf("expected preview cache to be cleared, got err=%v", err)
	}
	if _, err := cache.GetCache[string]("codex:usage:detail:7"); !errors.Is(err, cache.CacheNotFound) {
		t.Fatalf("expected detail cache to be cleared, got err=%v", err)
	}
}

func TestConsumeCodexResetCreditReportsCacheFailureAsWarningAfterUpstreamSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cache.InitCacheManager()
	channelID := 8
	if err := cache.SetCache("codex:usage:preview:8", "legacy", time.Minute); err != nil {
		t.Fatalf("failed to seed legacy cache: %v", err)
	}

	originalLoad := loadCodexUsageChannelByID
	originalCreate := createCodexUsageProvider
	originalClear := clearCodexUsageCacheForChannel
	loadCodexUsageChannelByID = func(int) (*model.Channel, error) {
		return &model.Channel{Id: channelID, Type: config.ChannelTypeCodex, Key: "credentials"}, nil
	}
	createCodexUsageProvider = func(*model.Channel) (codexUsageProvider, error) {
		return &fakeCodexUsageProvider{resetResult: &codex.CodexResetResult{ChannelID: channelID, WindowsReset: 1}}, nil
	}
	clearCodexUsageCacheForChannel = func(*model.Channel) error { return errors.New("cache unavailable") }
	t.Cleanup(func() {
		loadCodexUsageChannelByID = originalLoad
		createCodexUsageProvider = originalCreate
		clearCodexUsageCacheForChannel = originalClear
	})

	ctx, recorder := commonTest.GetContext(http.MethodPost, "/api/channel/8/codex/reset-credit", commonTest.RequestJSONConfig(), nil)
	ctx.Params = gin.Params{{Key: "id", Value: "8"}}
	ConsumeCodexResetCredit(ctx)

	var response struct {
		Success bool   `json:"success"`
		Warning string `json:"warning"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if !response.Success || !strings.Contains(response.Warning, "cache unavailable") {
		t.Fatalf("irreversible upstream success must remain successful with warning, body=%s", recorder.Body.String())
	}
	if _, err := cache.GetCache[string]("codex:usage:preview:8"); !errors.Is(err, cache.CacheNotFound) {
		t.Fatalf("legacy cleanup must still execute after v2 failure, got %v", err)
	}
}

func TestConsumeCodexResetCreditRejectsInvalidChannels(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name    string
		idParam string
		channel *model.Channel
		want    string
	}{
		{
			name:    "invalid id",
			idParam: "bad",
			want:    "invalid syntax",
		},
		{
			name:    "non codex",
			idParam: "8",
			channel: &model.Channel{Id: 8, Type: config.ChannelTypeOpenAI, Key: "credentials"},
			want:    "channel type is not Codex",
		},
		{
			name:    "empty key",
			idParam: "9",
			channel: &model.Channel{Id: 9, Type: config.ChannelTypeCodex, Key: "  "},
			want:    "Codex channel credentials are empty",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			originalLoadByID := loadCodexUsageChannelByID
			originalCreateProvider := createCodexUsageProvider
			t.Cleanup(func() {
				loadCodexUsageChannelByID = originalLoadByID
				createCodexUsageProvider = originalCreateProvider
			})

			provider := &fakeCodexUsageProvider{}

			loadCodexUsageChannelByID = func(channelID int) (*model.Channel, error) {
				if tt.channel == nil {
					return nil, errors.New("channel not found")
				}
				return tt.channel, nil
			}
			createCodexUsageProvider = func(channel *model.Channel) (codexUsageProvider, error) {
				return provider, nil
			}

			ctx, recorder := commonTest.GetContext(http.MethodPost, "/api/channel/"+tt.idParam+"/codex/reset-credit", commonTest.RequestJSONConfig(), nil)
			ctx.Params = gin.Params{{Key: "id", Value: tt.idParam}}

			ConsumeCodexResetCredit(ctx)

			if recorder.Code != http.StatusOK {
				t.Fatalf("expected status 200, got %d", recorder.Code)
			}

			var resp struct {
				Success bool   `json:"success"`
				Message string `json:"message"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
				t.Fatalf("failed to decode response: %v", err)
			}
			if resp.Success {
				t.Fatalf("expected failure response, got body %s", recorder.Body.String())
			}
			if !strings.Contains(resp.Message, tt.want) {
				t.Fatalf("expected message to contain %q, got %q", tt.want, resp.Message)
			}
			if provider.resetCalled {
				t.Fatal("expected invalid channel not to call reset provider method")
			}
		})
	}
}

func TestGetCodexUsagePreviewsAllowsTaggedChannels(t *testing.T) {
	gin.SetMode(gin.TestMode)

	originalLoadByIDs := loadCodexUsageChannelsByID
	originalCreateProvider := createCodexUsageProvider
	t.Cleanup(func() {
		loadCodexUsageChannelsByID = originalLoadByIDs
		createCodexUsageProvider = originalCreateProvider
	})

	provider := &fakeCodexUsageProvider{
		preview: &codex.CodexUsagePreview{
			ChannelID: 7,
			PlanType:  "plus",
		},
	}

	loadCodexUsageChannelsByID = func(ids []int) ([]*model.Channel, error) {
		if len(ids) != 1 || ids[0] != 7 {
			t.Fatalf("unexpected preview ids: %+v", ids)
		}
		return []*model.Channel{
			{
				Id:   7,
				Type: config.ChannelTypeCodex,
				Tag:  "codex-team",
				Key:  "credentials",
			},
		}, nil
	}
	createCodexUsageProvider = func(channel *model.Channel) (codexUsageProvider, error) {
		if channel.Tag != "codex-team" {
			t.Fatalf("expected tagged channel to reach preview provider creation, got tag %q", channel.Tag)
		}
		return provider, nil
	}

	ctx, recorder := commonTest.GetContext(
		http.MethodPost,
		"/api/channel/codex/usage/previews",
		commonTest.RequestJSONConfig(),
		strings.NewReader(`{"ids":[7]}`),
	)

	GetCodexUsagePreviews(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", recorder.Code)
	}

	var resp struct {
		Success bool `json:"success"`
		Data    struct {
			Items []codexUsagePreviewItem `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if !resp.Success {
		t.Fatalf("expected success response, got body %s", recorder.Body.String())
	}
	if len(resp.Data.Items) != 1 {
		t.Fatalf("expected one preview item, got %+v", resp.Data.Items)
	}
	if !resp.Data.Items[0].OK || resp.Data.Items[0].Preview == nil {
		t.Fatalf("expected tagged channel preview to succeed, got %+v", resp.Data.Items[0])
	}
	if resp.Data.Items[0].Preview.ChannelID != 7 {
		t.Fatalf("expected preview for channel 7, got %+v", resp.Data.Items[0].Preview)
	}
	if provider.previewForceRefresh {
		t.Fatalf("expected preview endpoint to use cached preview fetch")
	}
}

func TestGetCodexUsagePreviewsRejectsTooManyIDs(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ids := make([]int, codexUsagePreviewMaxIDs+1)
	for index := range ids {
		ids[index] = index + 1
	}

	payload, err := json.Marshal(map[string]any{
		"ids": ids,
	})
	if err != nil {
		t.Fatalf("failed to encode preview request payload: %v", err)
	}

	ctx, recorder := commonTest.GetContext(
		http.MethodPost,
		"/api/channel/codex/usage/previews",
		commonTest.RequestJSONConfig(),
		strings.NewReader(string(payload)),
	)

	GetCodexUsagePreviews(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", recorder.Code)
	}

	var resp struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Success {
		t.Fatalf("expected validation failure for oversized preview batch, got body %s", recorder.Body.String())
	}
	if !strings.Contains(resp.Message, "ids不能超过100个") {
		t.Fatalf("expected oversized batch message, got %q", resp.Message)
	}
}
