package controller

import (
	"encoding/json"
	"github.com/gin-gonic/gin"
	"io"
	"net/http"
	"net/http/httptest"
	"one-api/common/config"
	"one-api/model"
	"strings"
	"testing"
)

func TestUserSecurityDisabledGroupRateDoesNotPanic(t *testing.T) {
	userGroupControllerFixture(t)
	old := model.GlobalUserGroupRatio
	model.GlobalUserGroupRatio = &model.UserGroupRatio{}
	t.Cleanup(func() { model.GlobalUserGroupRatio = old })
	if err := model.DB.AutoMigrate(&model.PublicationVersion{}); err != nil {
		t.Fatal(err)
	}
	if err := model.EnsurePublicationVersionRows(model.DB); err != nil {
		t.Fatal(err)
	}
	if err := model.ChangeUserQuota(1, 1); err != nil {
		t.Fatal(err)
	}
	var group model.UserGroup
	if err := model.DB.Where("symbol = ?", "paid").First(&group).Error; err != nil {
		t.Fatal(err)
	}
	if err := model.ChangeUserGroupEnable(group.Id, false); err != nil {
		t.Fatal(err)
	}
	if model.GlobalUserGroupRatio.GetAPILimiter("paid") != nil {
		t.Fatal("测试需要已停用组")
	}
	r := gin.New()
	r.Use(gin.RecoveryWithWriter(io.Discard))
	r.GET("/rate", func(c *gin.Context) { c.Set("id", 1); GetRateRealtime(c) })
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/rate", nil))
	if w.Code == 500 {
		t.Fatalf("停用分组后速率查询 panic: status=%d body=%q", w.Code, w.Body.String())
	}
}
func TestUserSecurityRegisterRejectsEmptyUsername(t *testing.T) {
	userGroupControllerFixture(t)
	manager := config.GlobalOption
	for _, key := range []string{"RegisterEnabled", "PasswordRegisterEnabled", "EmailVerificationEnabled"} {
		v := false
		manager.RegisterBool(key, &v)
	}
	if _, err := manager.PublishRuntimeOverrides(2, map[string]string{"QuotaPerUnit": "1", "RegisterEnabled": "true", "PasswordRegisterEnabled": "true", "EmailVerificationEnabled": "false"}); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"password":"password123"}`, `{"username":"","password":"password123"}`, `{"username":"   ","password":"password123"}`} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/api/user/register", strings.NewReader(body))
		Register(c)
		var response struct{ Success bool }
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response.Success {
			t.Fatalf("无效用户名仍注册成功: %s", body)
		}
	}
	var count int64
	if err := model.DB.Model(&model.User{}).Where("TRIM(username) = ?", "").Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("创建了空用户名账户")
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/user/register", strings.NewReader(`{"username":"valid-user","password":"password123"}`))
	Register(c)
	var response struct{ Success bool }
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || !response.Success {
		t.Fatalf("正常注册失败: %s", w.Body.String())
	}
	user := model.User{Username: "valid-user", Password: "password123"}
	if err := user.ValidateAndFill(); err != nil {
		t.Fatalf("正常注册不能登录: %v", err)
	}
}
