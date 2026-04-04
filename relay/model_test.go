package relay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"one-api/common/groupctx"
	"one-api/model"

	"github.com/gin-gonic/gin"
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
