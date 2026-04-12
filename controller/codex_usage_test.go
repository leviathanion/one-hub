package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

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
	previewForceRefresh  bool
	snapshotForceRefresh bool
}

func (f *fakeCodexUsageProvider) GetUsagePreview(_ context.Context, forceRefresh bool) (*codex.CodexUsagePreview, error) {
	f.previewForceRefresh = forceRefresh
	return f.preview, f.previewErr
}

func (f *fakeCodexUsageProvider) GetUsageSnapshot(_ context.Context, forceRefresh bool) (*codex.CodexUsageSnapshot, error) {
	f.snapshotForceRefresh = forceRefresh
	return f.snapshot, f.snapshotErr
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
