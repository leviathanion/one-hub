package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"one-api/common/config"
	"one-api/common/groupctx"
	"one-api/model"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestGroupDistributorSetupGroupsPreservesTokenGroupAndInitializesRoutingGroup(t *testing.T) {
	gin.SetMode(gin.TestMode)

	originalDB := model.DB
	testDB, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("expected in-memory sqlite database, got %v", err)
	}
	if err := testDB.AutoMigrate(&model.User{}); err != nil {
		t.Fatalf("expected user schema migration, got %v", err)
	}
	model.DB = testDB
	t.Cleanup(func() {
		model.DB = originalDB
	})

	originalUserGroups := model.GlobalUserGroupRatio.UserGroup
	originalAPILimiter := model.GlobalUserGroupRatio.APILimiter
	originalPublicGroups := append([]string(nil), model.GlobalUserGroupRatio.PublicGroup...)
	model.GlobalUserGroupRatio.UserGroup = map[string]*model.UserGroup{
		"token-a":  {Symbol: "token-a", Ratio: 1.5},
		"user-a":   {Symbol: "user-a", Ratio: 1.25},
		"backup-a": {Symbol: "backup-a", Ratio: 1.75},
	}
	model.GlobalUserGroupRatio.APILimiter = nil
	model.GlobalUserGroupRatio.PublicGroup = nil
	t.Cleanup(func() {
		model.GlobalUserGroupRatio.UserGroup = originalUserGroups
		model.GlobalUserGroupRatio.APILimiter = originalAPILimiter
		model.GlobalUserGroupRatio.PublicGroup = originalPublicGroups
	})

	user := &model.User{
		Id:       99,
		Username: "tester",
		Password: "password123",
		Group:    "user-a",
	}
	if err := testDB.Create(user).Error; err != nil {
		t.Fatalf("expected test user insert, got %v", err)
	}

	newContext := func() *gin.Context {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/realtime", nil)
		ctx.Set("id", user.Id)
		return ctx
	}

	tokenCtx := newContext()
	tokenCtx.Set("token_group", "token-a")
	tokenCtx.Set("token_backup_group", "backup-a")
	if err := NewGroupDistributor(tokenCtx).SetupGroups(); err != nil {
		t.Fatalf("expected token-group setup to succeed, got %v", err)
	}
	if tokenCtx.GetString("token_group") != "token-a" {
		t.Fatalf("expected token_group to remain declared value, got %q", tokenCtx.GetString("token_group"))
	}
	if got := groupctx.CurrentRoutingGroup(tokenCtx); got != "token-a" {
		t.Fatalf("expected routing group to initialize from token group, got %q", got)
	}
	if got := groupctx.CurrentRoutingGroupSource(tokenCtx); got != groupctx.RoutingGroupSourceTokenGroup {
		t.Fatalf("expected token group source, got %q", got)
	}
	if got := tokenCtx.GetFloat64("group_ratio"); got != 1.5 {
		t.Fatalf("expected token group ratio to be applied, got %v", got)
	}

	userCtx := newContext()
	userCtx.Set("token_backup_group", "backup-a")
	if err := NewGroupDistributor(userCtx).SetupGroups(); err != nil {
		t.Fatalf("expected user-group fallback setup to succeed, got %v", err)
	}
	if userCtx.GetString("token_group") != "" {
		t.Fatalf("expected empty token_group to remain unchanged, got %q", userCtx.GetString("token_group"))
	}
	if got := groupctx.CurrentRoutingGroup(userCtx); got != "user-a" {
		t.Fatalf("expected routing group to initialize from user group, got %q", got)
	}
	if got := groupctx.CurrentRoutingGroupSource(userCtx); got != groupctx.RoutingGroupSourceUserGroup {
		t.Fatalf("expected user group source, got %q", got)
	}
	if got := userCtx.GetFloat64("group_ratio"); got != 1.25 {
		t.Fatalf("expected user group ratio to be applied, got %v", got)
	}
	if got := userCtx.GetString(config.GinRoutingGroupKey); got != "user-a" {
		t.Fatalf("expected routing group context key to be populated, got %q", got)
	}
}
