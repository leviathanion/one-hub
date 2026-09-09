package controller

import (
	"encoding/json"
	"github.com/gin-gonic/gin"
	"net/http"
	"net/http/httptest"
	"one-api/common"
	"one-api/common/config"
	"one-api/model"
	"strings"
	"testing"
)

func TestUserSecuritySelfPasswordPatchPreservesOmittedDisplayName(t *testing.T) {
	userGroupControllerFixture(t)
	if err := model.DB.Model(&model.User{}).Where("id = ?", 1).Update("display_name", "原显示名").Error; err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("id", 1)
	c.Request = httptest.NewRequest(http.MethodPut, "/api/user/self", strings.NewReader(`{"password":"new-password123"}`))
	UpdateSelf(c)
	var response struct{ Success bool }
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Success {
		t.Fatalf("修改密码失败: %s", w.Body.String())
	}
	user, err := model.GetUserById(1, true)
	if err != nil {
		t.Fatal(err)
	}
	if !common.ValidatePasswordAndHash("new-password123", user.Password) {
		t.Fatal("密码未更新")
	}
	if user.DisplayName != "原显示名" {
		t.Fatalf("只改密码却清空显示名: display_name=%q", user.DisplayName)
	}
}

func TestUserSecurityAdminPatchRejectsInvalidControlValues(t *testing.T) {
	for _, body := range []string{`{"id":1,"group":""}`, `{"id":1,"status":0}`, `{"id":1,"role":0}`, `{"id":1,"role":2}`, `{"id":1,"status":3}`, `{"id":1,"group":"missing"}`, `{"id":1,"group":"disabled","display_name":"不能保存"}`} {
		t.Run(body, func(t *testing.T) {
			userGroupControllerFixture(t)
			disabled := false
			if err := model.DB.Create(&model.UserGroup{Symbol: "disabled", Enable: &disabled}).Error; err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Set("role", config.RoleRootUser)
			c.Set("id", 99)
			c.Request = httptest.NewRequest(http.MethodPut, "/api/user/", strings.NewReader(body))
			UpdateUser(c)
			var response struct{ Success bool }
			_ = json.Unmarshal(w.Body.Bytes(), &response)
			user, err := model.GetUserById(1, false)
			if err != nil {
				t.Fatal(err)
			}
			if response.Success {
				t.Fatalf("接受无效控制值: group=%q status=%d role=%d", user.Group, user.Status, user.Role)
			}
			if user.Role != config.RoleCommonUser || user.Status != config.UserStatusEnabled || user.Group != "default" || user.DisplayName != "" {
				t.Fatalf("非法修改产生部分写入: %+v", user)
			}
		})
	}
}
