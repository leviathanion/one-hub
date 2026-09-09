package controller

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"one-api/common/config"
	"one-api/model"
)

func userGroupControllerFixture(t *testing.T) *model.User {
	t.Helper()
	useOrderCallbackTestDB(t)
	old := config.GlobalOption
	manager := config.NewOptionManager()
	unit := float64(1)
	manager.RegisterFloat("QuotaPerUnit", &unit)
	config.GlobalOption = manager
	t.Cleanup(func() { config.GlobalOption = old })
	if _, err := manager.PublishRuntimeOverrides(1, map[string]string{"QuotaPerUnit": "1"}); err != nil {
		t.Fatal(err)
	}
	u := &model.User{Username: "group-user", Password: "password123", Quota: 200, Group: "default", AccessToken: "fixture-access"}
	if err := model.DB.Create(u).Error; err != nil {
		t.Fatal(err)
	}
	if err := model.DB.Create(&model.UserGroup{Symbol: "paid", Promotion: true, Min: 150}).Error; err != nil {
		t.Fatal(err)
	}
	if err := model.DB.AutoMigrate(&model.UserVerification{}, &model.Option{}); err != nil {
		t.Fatal(err)
	}
	if err := model.DB.Create(&model.User{Id: 99, Username: "test-root", Password: "fixture-password", Role: config.RoleRootUser, AccessToken: "root-fixture-access", AffCode: "root-fixture-aff"}).Error; err != nil {
		t.Fatal(err)
	}
	return u
}

func TestAdminZeroQuotaRecalculatesWithoutChangingMoney(t *testing.T) {
	userGroupControllerFixture(t)
	for _, tc := range []struct {
		body    string
		success bool
	}{{`{}`, false}, {`{"quota":null}`, false}, {`{"quota":0.5}`, false}, {`{"quota":0}`, true}, {`{"quota":0}`, true}} {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Set("id", 99)
		c.Params = gin.Params{{Key: "id", Value: "1"}}
		c.Request = httptest.NewRequest(http.MethodPost, "/api/user/quota/1", strings.NewReader(tc.body))
		c.Request.Header.Set("Content-Type", "application/json")
		ChangeUserQuota(c)
		var response struct{ Success bool }
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response.Success != tc.success {
			t.Fatalf("body=%s response=%s", tc.body, recorder.Body.String())
		}
	}
	u, err := model.GetUserById(1, false)
	if err != nil || u.Quota != 200 || u.UsedQuota != 0 || u.Group != "paid" {
		t.Fatalf("重算修改资金或未生效: %+v %v", u, err)
	}
	var log model.Log
	if err := model.DB.Where("user_id = ? AND type = ?", 1, model.LogTypeManage).First(&log).Error; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.Content, "重新计算用户组") || !strings.Contains(log.Content, "default → paid") {
		t.Fatalf("缺少准确重算审计: %s", log.Content)
	}
}

func TestLoginUsesCurrentPrincipalAndDoesNotOverwritePromotion(t *testing.T) {
	stale := userGroupControllerFixture(t)
	if err := model.ChangeUserQuota(stale.Id, 1); err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	r.Use(sessions.Sessions("session", cookie.NewStore([]byte("test-session-secret"))))
	r.POST("/login", func(c *gin.Context) { setupLogin(stale, c) })
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/login", nil))
	var response struct{ Success bool }
	_ = json.Unmarshal(w.Body.Bytes(), &response)
	if !response.Success {
		t.Fatalf("登录失败: %s", w.Body.String())
	}
	u, err := model.GetUserById(stale.Id, false)
	if err != nil || u.Group != "paid" {
		t.Fatalf("登录覆盖新组: %+v %v", u, err)
	}
	const hook = "test:login-write-failure"
	if err := model.DB.Callback().Update().Before("gorm:update").Register(hook, func(tx *gorm.DB) {
		if tx.Statement.Table == "users" {
			tx.AddError(errors.New("login storage failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = model.DB.Callback().Update().Remove(hook) })
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/login", nil))
	_ = json.Unmarshal(w.Body.Bytes(), &response)
	if response.Success || len(w.Result().Cookies()) != 0 {
		t.Fatalf("失败仍返回成功会话: %s", w.Body.String())
	}
}

func TestAdminProfilePatchDoesNotWriteUnspecifiedGroup(t *testing.T) {
	userGroupControllerFixture(t)
	if err := model.ChangeUserQuota(1, 1); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("role", config.RoleRootUser)
	c.Set("id", 99)
	c.Request = httptest.NewRequest(http.MethodPut, "/api/user/", strings.NewReader(`{"id":1,"display_name":"改名","quota":999,"used_quota":999}`))
	UpdateUser(c)
	var response struct{ Success bool }
	_ = json.Unmarshal(w.Body.Bytes(), &response)
	if !response.Success {
		t.Fatalf("部分编辑失败: %s", w.Body.String())
	}
	u, err := model.GetUserById(1, false)
	if err != nil || u.Group != "paid" || u.Quota != 201 || u.UsedQuota != 0 || u.DisplayName != "改名" {
		t.Fatalf("资料编辑越界: %+v %v", u, err)
	}
}
