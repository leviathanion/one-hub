package relay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"one-api/common/config"
	"one-api/common/groupctx"
	"one-api/model"

	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"
)

func TestListModelsByTokenUsesCurrentRoutingGroup(t *testing.T) {
	gin.SetMode(gin.TestMode)

	channelGroupSnapshot := snapshotChannelGroup()
	t.Cleanup(func() {
		restoreChannelGroup(channelGroupSnapshot)
	})

	originalPricing := model.PricingInstance
	model.PricingInstance = &model.Pricing{
		Prices: map[string]*model.Price{},
	}
	t.Cleanup(func() {
		model.PricingInstance = originalPricing
	})

	originalModelOwnedBys := model.ModelOwnedBysInstance
	model.ModelOwnedBysInstance = &model.ModelOwnedBys{
		ModelOwnedBy: map[int]*model.ModelOwnedBy{},
	}
	t.Cleanup(func() {
		model.ModelOwnedBysInstance = originalModelOwnedBys
	})

	model.ChannelGroup = model.ChannelsChooser{
		Rule: map[string]map[string][][]int{
			"token-a": {
				"token-only-model": nil,
			},
			"user-a": {
				"user-only-model": nil,
			},
		},
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	ctx.Set("token_group", "token-a")
	ctx.Set("group", "user-a")
	groupctx.SetRoutingGroup(ctx, "user-a", groupctx.RoutingGroupSourceUserGroup)

	ListModelsByToken(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200 response, got %d", recorder.Code)
	}

	var response struct {
		Object string `json:"object"`
		Data   []struct {
			Id string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("expected JSON response, got %v", err)
	}

	if response.Object != "list" {
		t.Fatalf("expected list object type, got %q", response.Object)
	}
	if len(response.Data) != 1 || response.Data[0].Id != "user-only-model" {
		t.Fatalf("expected models for current routing group, got %#v", response.Data)
	}
}

func TestListClaudeModelsByTokenIncludesCustomClaudeRelayModels(t *testing.T) {
	gin.SetMode(gin.TestMode)

	channelGroupSnapshot := snapshotChannelGroup()
	t.Cleanup(func() {
		restoreChannelGroup(channelGroupSnapshot)
	})

	originalPricing := model.PricingInstance
	model.PricingInstance = &model.Pricing{
		Prices: map[string]*model.Price{
			"native-claude": {
				Model:       "native-claude",
				ChannelType: config.ChannelTypeAnthropic,
			},
			"custom-claude-model": {
				Model:       "custom-claude-model",
				ChannelType: config.ChannelTypeCustom,
			},
			"custom-openai-model": {
				Model:       "custom-openai-model",
				ChannelType: config.ChannelTypeCustom,
			},
			"priced-claude-custom-disabled": {
				Model:       "priced-claude-custom-disabled",
				ChannelType: config.ChannelTypeAnthropic,
			},
		},
	}
	t.Cleanup(func() {
		model.PricingInstance = originalPricing
	})

	weight := uint(1)
	proxy := ""
	enabledPlugin := datatypes.NewJSONType(model.PluginType{
		"claude": {
			"enabled": true,
		},
	})
	disabledPlugin := datatypes.NewJSONType(model.PluginType{
		"claude": {
			"enabled": false,
		},
	})

	model.ChannelGroup = model.ChannelsChooser{
		Channels: map[int]*model.ChannelChoice{
			10: {
				Channel: &model.Channel{
					Id:     10,
					Type:   config.ChannelTypeAnthropic,
					Weight: &weight,
					Proxy:  &proxy,
				},
			},
			11: {
				Channel: &model.Channel{
					Id:     11,
					Type:   config.ChannelTypeCustom,
					Weight: &weight,
					Proxy:  &proxy,
					Plugin: &enabledPlugin,
				},
			},
			12: {
				Channel: &model.Channel{
					Id:     12,
					Type:   config.ChannelTypeCustom,
					Weight: &weight,
					Proxy:  &proxy,
					Plugin: &disabledPlugin,
				},
			},
		},
		Rule: map[string]map[string][][]int{
			"default": {
				"native-claude":                 {{10}},
				"custom-claude-model":           {{11}},
				"custom-openai-model":           {{12}},
				"priced-claude-custom-disabled": {{12}},
			},
		},
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/claude/v1/models", nil)
	ctx.Set("token_group", "default")

	ListClaudeModelsByToken(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200 response, got %d", recorder.Code)
	}

	var response struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("expected JSON response, got %v", err)
	}

	var ids []string
	for _, item := range response.Data {
		ids = append(ids, item.ID)
	}

	expected := []string{"custom-claude-model", "native-claude"}
	if len(ids) != len(expected) {
		t.Fatalf("expected %v, got %v", expected, ids)
	}
	for index, expectedID := range expected {
		if ids[index] != expectedID {
			t.Fatalf("expected %v, got %v", expected, ids)
		}
	}
}
