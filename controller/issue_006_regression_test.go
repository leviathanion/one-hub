package controller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"one-api/model"
)

func TestIssue006AdminCreateAndUpdateFreeGroup(t *testing.T) {
	router := setupPricingControllerTest(t)
	if err := model.DB.AutoMigrate(&model.UserGroup{}); err != nil {
		t.Fatal(err)
	}
	previous := model.GlobalUserGroupRatio
	model.GlobalUserGroupRatio = &model.UserGroupRatio{}
	t.Cleanup(func() {
		for _, limiter := range model.GlobalUserGroupRatio.APILimiter {
			if stopper, ok := limiter.(interface{ Stop() }); ok {
				stopper.Stop()
			}
		}
		model.GlobalUserGroupRatio = previous
	})
	router.POST("/groups", AddUserGroup)
	router.PUT("/groups", UpdateUserGroup)
	write := func(method string, payload map[string]any, success bool) {
		t.Helper()
		body, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(method, "/groups", bytes.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(recorder, request)
		var response struct {
			Success bool `json:"success"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || response.Success != success {
			t.Fatalf("管理接口返回错误: %s err=%v", recorder.Body.String(), err)
		}
	}
	for _, test := range []struct {
		name  string
		ratio any
		want  float64
	}{
		{"free", 0, 0},
		{"positive", 2.5, 2.5},
		{"omitted", nil, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload := map[string]any{"symbol": test.name, "name": test.name}
			if test.ratio != nil {
				payload["ratio"] = test.ratio
			}
			write(http.MethodPost, payload, true)
			var group model.UserGroup
			if err := model.DB.Where("symbol = ?", test.name).First(&group).Error; err != nil {
				t.Fatal(err)
			}
			if group.Ratio != test.want || model.GlobalUserGroupRatio.GetBySymbol(test.name).Ratio != test.want {
				t.Fatalf("创建倍率=%v want=%v", group.Ratio, test.want)
			}
			payload["id"], payload["ratio"] = group.Id, 0
			write(http.MethodPut, payload, true)
			if err := model.DB.First(&group, group.Id).Error; err != nil || group.Ratio != 0 {
				t.Fatalf("更新零倍率失败: ratio=%v err=%v", group.Ratio, err)
			}
		})
	}
	write(http.MethodPost, map[string]any{"symbol": "invalid", "ratio": -1}, false)
}
