package middleware

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"one-api/common/config"
	"one-api/common/groupctx"
	"one-api/model"
	"one-api/types"

	"github.com/gin-gonic/gin"

	"gorm.io/gorm"
)

func TestFixI055_CurrentPrincipalRejectsSQLRevocationWithoutLongLivedMarker(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*gorm.DB) error
	}{
		{"token disabled", func(db *gorm.DB) error {
			return db.Model(&model.Token{}).Where("id = 1").Update("status", config.TokenStatusDisabled).Error
		}},
		{"token deleted", func(db *gorm.DB) error { return db.Delete(&model.Token{}, 1).Error }},
		{"token owner changed", func(db *gorm.DB) error { return db.Model(&model.Token{}).Where("id = 1").Update("user_id", 2).Error }},
		{"token expired", func(db *gorm.DB) error {
			return db.Model(&model.Token{}).Where("id = 1").Update("expired_time", time.Now().Unix()-1).Error
		}},
		{"user disabled", func(db *gorm.DB) error {
			return db.Model(&model.User{}).Where("id = 1").Update("status", config.UserStatusDisabled).Error
		}},
		{"user deleted", func(db *gorm.DB) error { return db.Delete(&model.User{}, 1).Error }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, db := setupLongLivedPrincipalTest(t)
			if IsAuthenticatedLongLivedPrincipal(ctx) {
				t.Fatal("夹具必须是无长连接标记的 HTTP 上下文")
			}
			if err := tt.mutate(db); err != nil {
				t.Fatal(err)
			}
			if apiErr := ValidateCurrentPrincipal(ctx); apiErr == nil || apiErr.StatusCode != http.StatusUnauthorized {
				t.Fatalf("SQL 中已失效的主体仍被接受: %+v", apiErr)
			}
		})
	}
}

func TestFixI055_CurrentPrincipalSQLFailureIsUnavailable(t *testing.T) {
	for _, table := range []string{"tokens", "users"} {
		t.Run(table, func(t *testing.T) {
			ctx, db := setupLongLivedPrincipalTest(t)
			if err := db.Callback().Query().Before("gorm:query").Register("i055:read_failure", func(tx *gorm.DB) {
				if tx.Statement.Table == table {
					tx.AddError(errors.New("injected principal SQL failure"))
				}
			}); err != nil {
				t.Fatal(err)
			}
			defer db.Callback().Query().Remove("i055:read_failure")
			if apiErr := ValidateCurrentPrincipal(ctx); apiErr == nil || apiErr.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("SQL 读取错误不能回退缓存放行: %+v", apiErr)
			}
		})
	}
}

func TestFixI055_CurrentPrincipalHasNoWalletOrGroupAdmission(t *testing.T) {
	ctx, db := setupLongLivedPrincipalTest(t)
	if err := db.Model(&model.User{}).Where("id = 1").Updates(map[string]any{"quota": 0, "group": "removed-group"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.Token{}).Where("id = 1").Updates(map[string]any{"remain_quota": 0, "group": "unauthorized-group"}).Error; err != nil {
		t.Fatal(err)
	}
	groupctx.SetRoutingGroup(ctx, "owner-group", groupctx.RoutingGroupSourceBackupGroup)
	ctx.Set("group_ratio", float64(2))
	writes := 0
	if err := db.Callback().Update().Before("gorm:update").Register("i055:no_money_write", func(*gorm.DB) { writes++ }); err != nil {
		t.Fatal(err)
	}
	defer db.Callback().Update().Remove("i055:no_money_write")
	if apiErr := ValidateCurrentPrincipal(ctx); apiErr != nil {
		t.Fatalf("免费资源凭据有效性不应检查余额或新工作选组权限: %+v", apiErr)
	}
	if writes != 0 || groupctx.CurrentRoutingGroup(ctx) != "owner-group" || ctx.GetFloat64("group_ratio") != 2 {
		t.Fatalf("凭据检查修改了结算或选路: writes=%d group=%s ratio=%v", writes, groupctx.CurrentRoutingGroup(ctx), ctx.GetFloat64("group_ratio"))
	}
}

func TestFixI055_CurrentPrincipalUsesCurrentIPPolicy(t *testing.T) {
	ctx, db := setupLongLivedPrincipalTest(t)
	ctx.Request.RemoteAddr = "192.0.2.5:1234"
	token, err := model.GetTokenById(1)
	if err != nil {
		t.Fatal(err)
	}
	setting := token.Setting.Data()
	setting.Limits.LimitsIPSetting.Enabled = true
	setting.Limits.LimitsIPSetting.Whitelist = []string{"192.0.2.99"}
	token.Setting.Set(setting)
	if err := db.Session(&gorm.Session{SkipHooks: true}).Model(token).Update("setting", token.Setting).Error; err != nil {
		t.Fatal(err)
	}
	ctx.Set("token_setting", &model.TokenSetting{})
	if apiErr := ValidateCurrentPrincipal(ctx); apiErr == nil || apiErr.StatusCode != http.StatusForbidden {
		t.Fatalf("当前 SQL IP 限制必须覆盖旧缓存无限制设置: %+v", apiErr)
	}
	setting.Limits.LimitsIPSetting.Whitelist = []string{"192.0.2.0/24"}
	token.Setting.Set(setting)
	if err := db.Session(&gorm.Session{SkipHooks: true}).Model(token).Update("setting", token.Setting).Error; err != nil {
		t.Fatal(err)
	}
	if apiErr := ValidateCurrentPrincipal(ctx); apiErr != nil {
		t.Fatalf("当前 SQL 恢复授权后应允许请求: %+v", apiErr)
	}
}

func TestFixI055_CurrentPrincipalRespectsHTTPClientCancellation(t *testing.T) {
	ctx, _ := setupLongLivedPrincipalTest(t)
	requestCtx, cancel := context.WithCancel(ctx.Request.Context())
	cancel()
	ctx.Request = ctx.Request.WithContext(requestCtx)
	if apiErr := ValidateCurrentPrincipal(ctx); apiErr == nil || apiErr.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("取消的免费 HTTP 请求不应继续读取 SQL 放行: %+v", apiErr)
	}
}

func TestFixI055_HTTPWorkAdmissionRespectsClientCancellation(t *testing.T) {
	for _, phase := range []struct {
		name string
		run  func(*gin.Context) *types.OpenAIErrorWithStatusCode
	}{
		{"before selection", RefreshLongLivedPrincipal},
		{"after selection", func(c *gin.Context) *types.OpenAIErrorWithStatusCode {
			return AdmitAuthenticatedChannelWork(c, "gpt-live", 7)
		}},
	} {
		t.Run(phase.name, func(t *testing.T) {
			ctx, _ := setupLongLivedPrincipalTest(t)
			markLongLivedPrincipalAuthenticated(ctx)
			requestCtx, cancel := context.WithCancel(ctx.Request.Context())
			cancel()
			ctx.Request = ctx.Request.WithContext(requestCtx)
			if apiErr := phase.run(ctx); apiErr == nil || apiErr.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("HTTP input_tokens 的准入必须保留请求取消: %+v", apiErr)
			}
			if ctx.GetBool(longLivedPrincipalCurrentKey) {
				t.Fatal("取消后不能保留当前授权标记")
			}
		})
	}
}
