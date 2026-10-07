package relay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"one-api/common/config"
	"one-api/common/groupctx"
	"one-api/common/logger"
	"one-api/model"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/datatypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
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

	setupModelDisplayDB(t)

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
				Model: "native-claude",
			},
			"custom-claude-model": {
				Model: "custom-claude-model",
			},
			"custom-openai-model": {
				Model: "custom-openai-model",
			},
			"priced-claude-custom-disabled": {
				Model: "priced-claude-custom-disabled",
			},
		},
	}
	t.Cleanup(func() {
		model.PricingInstance = originalPricing
	})

	weight := uint(1)
	proxy := ""
	enabledPlugin := datatypes.NewJSONType(model.PluginType{
		"endpoints": {"anthropic.messages": map[string]any{
			"enabled": true,
		}},
	})
	disabledPlugin := datatypes.NewJSONType(model.PluginType{
		"endpoints": {"anthropic.messages": map[string]any{
			"enabled": false,
		}},
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

func setupModelDisplayDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "catalog.db")
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.ModelInfo{}, &model.ModelOwnedBy{}); err != nil {
		t.Fatal(err)
	}
	originalDB, originalLogger := model.DB, logger.Logger
	model.DB, logger.Logger = db, zap.NewNop()
	t.Cleanup(func() {
		model.DB, logger.Logger = originalDB, originalLogger
		sqlDB, _ := db.DB()
		_ = sqlDB.Close()
	})
	return db
}

func TestRetrieveModelUsesRoutingScopeWithoutPriceOrOwner(t *testing.T) {
	setupModelDisplayDB(t)
	snapshot := snapshotChannelGroup()
	t.Cleanup(func() { restoreChannelGroup(snapshot) })
	model.ChannelGroup = model.ChannelsChooser{
		Channels: map[int]*model.ChannelChoice{
			1: {Channel: &model.Channel{Id: 1, Type: config.ChannelTypeOpenAI}},
			2: {Channel: &model.Channel{Id: 2, Type: config.ChannelTypeOpenAI}},
		},
		Rule: map[string]map[string][][]int{
			"public":  {"unclassified": {{1}}, "public-*": {{1}}},
			"private": {"private-model": {{2}}, "private-*": {{2}}},
		},
		Match: []string{"public-*", "private-*"},
	}
	for _, name := range []string{"unclassified", "public-v1", "private-model", "private-v1", "missing"} {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(r)
			c.Request = httptest.NewRequest(http.MethodGet, "/v1/models/"+name, nil)
			c.Params = gin.Params{{Key: "model", Value: name}}
			c.Set("group", "private")
			c.Set("token_group", "public")
			RetrieveModel(c)
			var body map[string]any
			if err := json.Unmarshal(r.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if name == "unclassified" || name == "public-v1" {
				if body["id"] != name || body["owned_by"] != model.UnknownOwnedBy || body["error"] != nil {
					t.Fatalf("unknown owner must not hide a visible model: %s", r.Body.String())
				}
			} else if body["error"] == nil || body["id"] != nil {
				t.Fatalf("out-of-scope model was exposed: %s", r.Body.String())
			}
		})
	}
}

func TestListGeminiModelsUsesChannelCapabilityAndCurrentGroup(t *testing.T) {
	snapshot := snapshotChannelGroup()
	t.Cleanup(func() { restoreChannelGroup(snapshot) })
	model.ChannelGroup = model.ChannelsChooser{
		Channels: map[int]*model.ChannelChoice{
			1: {Channel: &model.Channel{Id: 1, Type: config.ChannelTypeGemini}},
			2: {Channel: &model.Channel{Id: 2, Type: config.ChannelTypeVertexAI}},
			3: {Channel: &model.Channel{Id: 3, Type: config.ChannelTypeOpenAI}},
		},
		Rule: map[string]map[string][][]int{
			"public":  {"gemini-alias": {{1}}, "vertex-alias": {{2}}, "gemini-branded-openai": {{3}}},
			"private": {"private-gemini": {{1}}},
		},
	}
	r := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(r)
	c.Request = httptest.NewRequest(http.MethodGet, "/gemini/v1beta/models", nil)
	c.Set("token_group", "public")
	ListGeminiModelsByToken(c)
	var body struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Models) != 2 || body.Models[0].Name != "models/gemini-alias" || body.Models[1].Name != "models/vertex-alias" {
		t.Fatalf("wrong protocol/group eligibility: %s", r.Body.String())
	}
}

func TestAvailableModelsComposeExactMetadataWithWildcardPrice(t *testing.T) {
	db := setupModelDisplayDB(t)
	ownerA, ownerB := 1001, 1002
	if err := db.Create(&[]model.ModelOwnedBy{{Id: ownerA, Name: "分类 A"}, {Id: ownerB, Name: "分类 B"}}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&[]model.ModelInfo{
		{Model: "shared-a", Name: "A", OwnedByID: &ownerA},
		{Model: "shared-b", Name: "B", OwnedByID: &ownerB},
	}).Error; err != nil {
		t.Fatal(err)
	}
	snapshot := snapshotChannelGroup()
	originalPricing, originalGroups := model.PricingInstance, model.GlobalUserGroupRatio
	t.Cleanup(func() {
		restoreChannelGroup(snapshot)
		model.PricingInstance, model.GlobalUserGroupRatio = originalPricing, originalGroups
	})
	price := &model.Price{Model: "shared-*", Input: 1, Output: 2}
	model.PricingInstance = &model.Pricing{Prices: map[string]*model.Price{"shared-*": price}, Match: []string{"shared-*"}}
	model.GlobalUserGroupRatio = &model.UserGroupRatio{PublicGroup: []string{"public"}}
	model.ChannelGroup = model.ChannelsChooser{ModelGroup: map[string]map[string]bool{
		"shared-a":      {"public": true, "private": true},
		"shared-b":      {"public": true},
		"shared-secret": {"private": true},
	}}
	models, err := GetAvailableModels("")
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 {
		t.Fatalf("private model exposed: %#v", models)
	}
	for name, expected := range map[string]string{"shared-a": "分类 A", "shared-b": "分类 B"} {
		item := models[name]
		if item == nil || item.OwnedBy != expected || item.Price.Model != "shared-*" || item.Price.ModelInfo.Model != name || len(item.Groups) != 1 || item.Groups[0] != "public" {
			t.Fatalf("incorrect composed model %s: %#v", name, item)
		}
	}
	if price.ModelInfo != nil || price.Model != "shared-*" {
		t.Fatal("display composition modified shared price policy")
	}
	// Simulate another process writing the same database without publishing prices.
	other, err := gorm.Open(sqlite.Open(db.Dialector.(*sqlite.Dialector).DSN), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlOther, _ := other.DB()
	defer sqlOther.Close()
	if err := other.Model(&model.ModelInfo{}).Where("model = ?", "shared-a").Updates(map[string]any{"name": "新名称", "owned_by_id": ownerB}).Error; err != nil {
		t.Fatal(err)
	}
	if err := other.Model(&model.ModelOwnedBy{}).Where("id = ?", ownerB).Update("name", "新分类").Error; err != nil {
		t.Fatal(err)
	}
	models, err = GetAvailableModels("")
	if err != nil {
		t.Fatal(err)
	}
	if models["shared-a"].OwnedBy != "新分类" || models["shared-a"].Price.ModelInfo.Name != "新名称" {
		t.Fatalf("metadata refresh incorrectly depends on price publication: %#v", models["shared-a"])
	}
}

func TestModelCatalogFailureIsNotReportedAsMissingMetadata(t *testing.T) {
	db := setupModelDisplayDB(t)
	if err := db.Migrator().DropTable(&model.ModelInfo{}); err != nil {
		t.Fatal(err)
	}
	snapshot := snapshotChannelGroup()
	t.Cleanup(func() { restoreChannelGroup(snapshot) })
	model.ChannelGroup = model.ChannelsChooser{Rule: map[string]map[string][][]int{"public": {"visible": nil}}}
	r := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(r)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	c.Set("token_group", "public")
	ListModelsByToken(c)
	if r.Code != http.StatusInternalServerError {
		t.Fatalf("catalog read failure was hidden: %d %s", r.Code, r.Body.String())
	}
}
