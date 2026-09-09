package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"one-api/common/config"
	"one-api/common/groupctx"
	"one-api/internal/testutil/sqlitetest"
	"one-api/model"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func setupLongLivedPrincipalTest(t *testing.T) (*gin.Context, *gorm.DB) {
	t.Helper()
	originalDB := model.DB
	originalGroups := model.GlobalUserGroupRatio
	model.GlobalUserGroupRatio = &model.UserGroupRatio{}

	db, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.Token{}, &model.UserGroup{}, &model.PublicationVersion{}); err != nil {
		t.Fatal(err)
	}
	model.DB = db
	t.Cleanup(func() {
		model.DB = originalDB
		model.GlobalUserGroupRatio = originalGroups
	})
	enabled := true
	if err := db.Create(&model.UserGroup{Symbol: "paid", Name: "Paid", Ratio: 2, APIRate: 600, Enable: &enabled}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.User{Id: 1, Username: "live-user", Password: "password123", AccessToken: "live-access", AffCode: "live-aff", Status: config.UserStatusEnabled, Group: "paid"}).Error; err != nil {
		t.Fatal(err)
	}
	token := &model.Token{Id: 1, UserId: 1, Key: "live-token", Status: config.TokenStatusEnabled, ExpiredTime: -1, UnlimitedQuota: false}
	token.Setting.Set(model.TokenSetting{Limits: model.LimitsConfig{LimitModelSetting: model.LimitModelSetting{Enabled: true, Models: []string{"gpt-live"}}}})
	if err := db.Session(&gorm.Session{SkipHooks: true}).Create(token).Error; err != nil {
		t.Fatal(err)
	}
	if err := model.EnsurePublicationVersionRows(db); err != nil {
		t.Fatal(err)
	}
	if err := model.GlobalUserGroupRatio.Load(); err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/realtime", nil)
	ctx.Set("id", 1)
	ctx.Set("token_id", 1)
	ctx.Set("token_unlimited_quota", true)
	ctx.Set("group", "stale")
	return ctx, db
}

func TestRefreshLongLivedPrincipalUsesAuthoritativeMutablePolicy(t *testing.T) {
	ctx, _ := setupLongLivedPrincipalTest(t)
	if apiErr := RefreshLongLivedPrincipal(ctx); apiErr != nil {
		t.Fatalf("refresh live principal: %v", apiErr)
	}
	if ctx.GetBool("token_unlimited_quota") {
		t.Fatal("stale unlimited flag was not replaced by SQL state")
	}
	if ctx.GetString("group") != "paid" || groupctx.CurrentRoutingGroup(ctx) != "paid" || ctx.GetFloat64("group_ratio") != 2 {
		t.Fatalf("live group policy was not refreshed: group=%q routing=%q ratio=%v", ctx.GetString("group"), groupctx.CurrentRoutingGroup(ctx), ctx.GetFloat64("group_ratio"))
	}
	if err := EnsureTokenModelAllowed(ctx, "gpt-live"); err != nil {
		t.Fatalf("live token model permission rejected: %v", err)
	}
	if err := EnsureTokenModelAllowed(ctx, "gpt-revoked"); err == nil {
		t.Fatal("live token model permission did not reject unlisted model")
	}
}

func TestRefreshLongLivedPrincipalRejectsRevocationAndExpiry(t *testing.T) {
	t.Run("disabled user", func(t *testing.T) {
		ctx, db := setupLongLivedPrincipalTest(t)
		if err := db.Model(&model.User{}).Where("id = ?", 1).Update("status", config.UserStatusDisabled).Error; err != nil {
			t.Fatal(err)
		}
		if apiErr := RefreshLongLivedPrincipal(ctx); apiErr == nil || apiErr.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expected disabled user rejection, got %+v", apiErr)
		}
	})
	t.Run("expired token", func(t *testing.T) {
		ctx, db := setupLongLivedPrincipalTest(t)
		if err := db.Model(&model.Token{}).Where("id = ?", 1).Update("expired_time", int64(0)).Error; err != nil {
			t.Fatal(err)
		}
		if apiErr := RefreshLongLivedPrincipal(ctx); apiErr == nil || apiErr.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expected expired token rejection, got %+v", apiErr)
		}
	})
}
