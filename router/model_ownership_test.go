package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"one-api/common/config"
	"one-api/internal/testutil/sqlitetest"
	"one-api/model"
	"strings"
	"testing"

	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestModelOwnershipRoutesRequireAdminAndKeepPublicCatalogSeparate(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.ModelInfo{}, &model.ModelOwnedBy{}, &model.Price{}, &model.Channel{}); err != nil {
		t.Fatal(err)
	}
	oldDB, oldRedisEnabled := model.DB, config.RedisEnabled
	model.DB, config.RedisEnabled = db, false
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		model.DB, config.RedisEnabled = oldDB, oldRedisEnabled
		_ = sqlDB.Close()
	})
	user := model.User{Username: "owner-viewer", Role: config.RoleCommonUser, Status: config.UserStatusEnabled}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.Price{Model: "price-only", Type: "tokens", Input: 1, Output: 2}).Error; err != nil {
		t.Fatal(err)
	}
	engine := gin.New()
	engine.Use(sessions.Sessions("session", cookie.NewStore([]byte("ownership-route-test-key"))))
	engine.Use(func(c *gin.Context) {
		if c.Request.Header.Get("X-Test-Session") != "" {
			session := sessions.Default(c)
			session.Set("username", user.Username)
			session.Set("id", user.Id)
		}
		c.Next()
	})
	SetApiRouter(engine)
	request := func(method, path, body string, signedIn bool) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if signedIn {
			req.Header.Set("X-Test-Session", "yes")
		}
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, req)
		return recorder
	}
	for _, route := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/model_info/ownership/catalog", ""},
		{http.MethodPost, "/api/model_info/ownership/suggestions", `{"models":["price-only"]}`},
		{http.MethodPost, "/api/model_info/ownership/batch", `{"mode":"replace","items":[{"model":"price-only","owned_by_id":null}]}`},
	} {
		for _, signedIn := range []bool{false, true} {
			response := request(route.method, route.path, route.body, signedIn)
			var envelope struct {
				Success bool   `json:"success"`
				Message string `json:"message"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || envelope.Success || !strings.Contains(envelope.Message, "无权") {
				t.Fatalf("unprivileged request reached %s: %d %s", route.path, response.Code, response.Body.String())
			}
		}
	}
	public := request(http.MethodGet, "/api/model_info/", "", false)
	if public.Code != http.StatusOK || !strings.Contains(public.Body.String(), `"success":true`) || strings.Contains(public.Body.String(), "price-only") {
		t.Fatalf("public catalog gained admin candidates: %s", public.Body.String())
	}
	if err := db.Model(&user).Update("role", config.RoleAdminUser).Error; err != nil {
		t.Fatal(err)
	}
	admin := request(http.MethodGet, "/api/model_info/ownership/catalog", "", true)
	if admin.Code != http.StatusOK || !strings.Contains(admin.Body.String(), `"model":"price-only"`) || !strings.Contains(admin.Body.String(), `"has_info":false`) {
		t.Fatalf("admin catalog missing price-only candidate: %d %s", admin.Code, admin.Body.String())
	}
}
