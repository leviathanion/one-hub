package controller

import (
	"encoding/json"
	"errors"
	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"net/http"
	"net/http/httptest"
	"one-api/common"
	"one-api/common/config"
	"one-api/middleware"
	"one-api/model"
	"strings"
	"testing"
)

func TestUserSecurityRevokedAdminSession(t *testing.T) {
	for _, mode := range []string{"demoted", "disabled", "deleted"} {
		t.Run(mode, func(t *testing.T) {
			u := userGroupControllerFixture(t)
			if err := model.DB.Model(&model.User{}).Where("id = ?", u.Id).Update("role", config.RoleAdminUser).Error; err != nil {
				t.Fatal(err)
			}
			r := gin.New()
			r.Use(sessions.Sessions("session", cookie.NewStore([]byte("review-admin-session-secret"))))
			r.POST("/login", func(c *gin.Context) { setupLogin(u, c) })
			r.GET("/admin", middleware.AdminAuth(), func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"success": true, "protected_handler": true}) })
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/login", nil))
			cookies := w.Result().Cookies()
			if len(cookies) == 0 {
				t.Fatalf("未取得合法登录会话: %s", w.Body.String())
			}
			var err error
			switch mode {
			case "demoted":
				err = model.DB.Model(&model.User{}).Where("id = ?", u.Id).Update("role", config.RoleCommonUser).Error
			case "disabled":
				err = model.DB.Model(&model.User{}).Where("id = ?", u.Id).Update("status", config.UserStatusDisabled).Error
			case "deleted":
				err = u.Delete()
			}
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodGet, "/admin", nil)
			for _, cookie := range cookies {
				req.AddCookie(cookie)
			}
			w = httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if strings.Contains(w.Body.String(), `"protected_handler":true`) {
				t.Fatalf("%s 后旧 cookie 仍进入管理接口: %s", mode, w.Body.String())
			}
		})
	}
}
func TestUserSecurityDeleteFailureResponse(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "sql_failure"}[fail], func(t *testing.T) {
			userGroupControllerFixture(t)
			const hook = "review:delete-failure"
			if fail {
				if err := model.DB.Callback().Delete().Before("gorm:delete").Register(hook, func(tx *gorm.DB) { tx.AddError(errors.New("injected delete failure")) }); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = model.DB.Callback().Delete().Remove(hook) })
			}
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Set("role", config.RoleRootUser)
			c.Set("id", 99)
			c.Params = gin.Params{{Key: "id", Value: "1"}}
			c.Request = httptest.NewRequest(http.MethodDelete, "/api/user/1", nil)
			DeleteUser(c)
			var body struct{ Success bool }
			err := json.Unmarshal(w.Body.Bytes(), &body)
			var count int64
			if err := model.DB.Model(&model.User{}).Where("id = ?", 1).Count(&count).Error; err != nil {
				t.Fatal(err)
			}
			if err != nil || body.Success == fail {
				t.Fatalf("fail=%v 剩余用户=%d HTTP=%d body=%q decode=%v", fail, count, w.Code, w.Body.String(), err)
			}
		})
	}
}
func TestUserAdminRejectsStaleAuthority(t *testing.T) {
	for _, mode := range []string{"target_promoted", "actor_demoted", "actor_disabled", "actor_deleted"} {
		t.Run(mode, func(t *testing.T) {
			userGroupControllerFixture(t)
			actor := model.User{Id: 98, Username: "test-admin", Password: "fixture-password", Role: config.RoleAdminUser, AccessToken: "admin-fixture-access", AffCode: "admin-fixture-aff"}
			if err := model.DB.Create(&actor).Error; err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "target_promoted":
				model.DB.Model(&model.User{}).Where("id = ?", 1).Update("role", config.RoleRootUser)
			case "actor_demoted":
				model.DB.Model(&actor).Update("role", config.RoleCommonUser)
			case "actor_disabled":
				model.DB.Model(&actor).Update("status", config.UserStatusDisabled)
			case "actor_deleted":
				model.DB.Delete(&actor)
			}
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Set("id", 98)
			c.Set("role", config.RoleAdminUser)
			c.Request = httptest.NewRequest(http.MethodPut, "/api/user/", strings.NewReader(`{"id":1,"password":"attacker-pass123"}`))
			UpdateUser(c)
			u, err := model.GetUserById(1, true)
			if err != nil {
				t.Fatal(err)
			}
			if common.ValidatePasswordAndHash("attacker-pass123", u.Password) {
				t.Fatalf("旧权限仍能修改密码: %s", w.Body.String())
			}
		})
	}
}
