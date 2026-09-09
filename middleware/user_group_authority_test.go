package middleware

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"one-api/common/cache"
	"one-api/common/config"
	"one-api/common/groupctx"
	commonredis "one-api/common/redis"
	"one-api/internal/testutil/fakeredis"
	"one-api/model"

	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func TestCreditGroupAuthoritySurvivesFailedInvalidationAndLateCacheFill(t *testing.T) {
	live, db := setupLongLivedPrincipalTest(t)
	if err := db.AutoMigrate(&model.Redemption{}, &model.Log{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.UserGroup{Symbol: "default", Ratio: 1, APIRate: 60, Public: true}).Error; err != nil {
		t.Fatal(err)
	}
	paid, err := model.GetUserGroupsById(1)
	if err != nil {
		t.Fatal(err)
	}
	paid.Promotion, paid.Min = true, 150
	if err := paid.Update(); err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.User{}).Where("id = ?", 1).Updates(map[string]any{"group": "default", "quota": 20}).Error; err != nil {
		t.Fatal(err)
	}
	oldOptions := config.GlobalOption
	manager := config.NewOptionManager()
	perUnit := float64(1)
	manager.RegisterFloat("QuotaPerUnit", &perUnit)
	config.GlobalOption = manager
	if _, err := manager.PublishRuntimeOverrides(1, map[string]string{"QuotaPerUnit": "1"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { config.GlobalOption = oldOptions })
	if apiErr := RefreshLongLivedPrincipal(live); apiErr != nil {
		t.Fatal(apiErr)
	}
	if groupctx.CurrentRoutingGroup(live) != "default" {
		t.Fatal("预期连接先使用旧组")
	}

	server, err := fakeredis.Start()
	if err != nil {
		t.Fatal(err)
	}
	oldRedisEnabled, oldRedis := config.RedisEnabled, commonredis.RDB
	config.RedisEnabled, commonredis.RDB = true, server.Client()
	cache.InitCacheManager()
	t.Cleanup(func() {
		_ = commonredis.RDB.Close()
		_ = server.Close()
		config.RedisEnabled, commonredis.RDB = oldRedisEnabled, oldRedis
		cache.InitCacheManager()
	})
	key := fmt.Sprintf("user_group:%d", 1)
	if err := cache.SetCache(key, "default", time.Minute); err != nil {
		t.Fatal(err)
	}
	code := &model.Redemption{Key: "authority-code", UserId: 99, Name: "充值", Quota: 150}
	if err := code.Insert(); err != nil {
		t.Fatal(err)
	}
	server.FailNext("DEL", "ERR injected deletion failure")
	if quota, err := model.Redeem(code.Key, 1, "test"); err != nil || quota != 150 {
		t.Fatalf("兑换: quota=%d err=%v", quota, err)
	}
	if cached, err := cache.GetCache[string](key); err != nil || cached != "default" {
		t.Fatalf("预期保留旧缓存: %q %v", cached, err)
	}

	// 独立节点快照模拟后处理之后的另一实例；读取不依赖旧节点内存。
	model.GlobalUserGroupRatio = &model.UserGroupRatio{}
	assertCurrentGroup := func() {
		t.Helper()
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		c.Set("id", 1)
		userReads := 0
		const callback = "test:count-authoritative-user-reads"
		if err := db.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
			if tx.Statement.Table == "users" {
				userReads++
			}
		}); err != nil {
			t.Fatal(err)
		}
		if err := NewGroupDistributor(c).SetupGroups(); err != nil {
			t.Fatal(err)
		}
		_ = db.Callback().Query().Remove(callback)
		if userReads != 1 {
			t.Fatalf("一次请求重复读主体: %d", userReads)
		}
		if groupctx.CurrentRoutingGroup(c) != "paid" || c.GetFloat64("group_ratio") != 2 {
			t.Fatalf("旧缓存覆盖新请求: %s %v", groupctx.CurrentRoutingGroup(c), c.GetFloat64("group_ratio"))
		}
		if apiErr := RefreshLongLivedPrincipal(live); apiErr != nil {
			t.Fatal(apiErr)
		}
		if groupctx.CurrentRoutingGroup(live) != "paid" {
			t.Fatalf("既有连接下一工作仍使用旧组: %s", groupctx.CurrentRoutingGroup(live))
		}
	}
	assertCurrentGroup()
	// 旧查询在提交和缓存删除以后晚回填。
	if err := cache.SetCache(key, "default", time.Minute); err != nil {
		t.Fatal(err)
	}
	assertCurrentGroup()
	if _, err := model.Redeem(code.Key, 1, "test"); err == nil {
		t.Fatal("缓存失败导致重复入账")
	}
	user, err := model.GetUserById(1, false)
	if err != nil || user.Quota != 170 {
		t.Fatalf("资金应只增加一次: %+v %v", user, err)
	}

	if err := db.Model(&model.Token{}).Where("id = ?", 1).Updates(map[string]any{"group": "default", "backup_group": "paid"}).Error; err != nil {
		t.Fatal(err)
	}
	if apiErr := RefreshLongLivedPrincipal(live); apiErr != nil {
		t.Fatal(apiErr)
	}
	if live.GetString("group") != "paid" || groupctx.CurrentRoutingGroup(live) != "default" || live.GetString("token_backup_group") != "paid" {
		t.Fatal("晋级覆盖了 token 显式分组优先级")
	}
}

func TestGroupAuthorityReadFailureRejectsNewAndLongLivedWork(t *testing.T) {
	live, db := setupLongLivedPrincipalTest(t)
	const callback = "test:fail-authoritative-user-read"
	if err := db.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "users" {
			tx.AddError(errors.New("injected read failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Callback().Query().Remove(callback) })
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c.Set("id", 1)
	c.Set("group", "paid")
	if err := NewGroupDistributor(c).SetupGroups(); err == nil || !c.IsAborted() || c.Writer.Status() != http.StatusServiceUnavailable {
		t.Fatalf("SQL 失败仍放行: %v status=%d", err, c.Writer.Status())
	}
	if apiErr := RefreshLongLivedPrincipal(live); apiErr == nil || apiErr.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("长连接SQL失败仍放行: %+v", apiErr)
	}
}

func TestSessionGroupAuthorityReadFailureDoesNotContinueAsAnonymous(t *testing.T) {
	_, db := setupLongLivedPrincipalTest(t)
	const callback = "test:session-group-read-failure"
	if err := db.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "users" {
			tx.AddError(errors.New("injected read failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Callback().Query().Remove(callback) })
	router := gin.New()
	router.Use(sessions.Sessions("session", cookie.NewStore([]byte("group-authority-test"))))
	router.Use(func(c *gin.Context) { sessions.Default(c).Set("id", 1); c.Next() })
	continued := false
	router.GET("/pricing", TrySetUserBySession(), func(c *gin.Context) { continued = true; c.Status(http.StatusOK) })
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/pricing", nil))
	if continued || recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("会话归属失败使用默认权限: continued=%v status=%d", continued, recorder.Code)
	}
}

func TestLongLivedWorkRejectsUnavailableGroupPublication(t *testing.T) {
	live, db := setupLongLivedPrincipalTest(t)
	if err := db.Migrator().DropTable(&model.PublicationVersion{}); err != nil {
		t.Fatal(err)
	}
	if apiErr := RefreshLongLivedPrincipal(live); apiErr == nil || apiErr.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("既有连接绕过策略可用性检查: %+v", apiErr)
	}
}
