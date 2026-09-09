package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"one-api/model"
	"one-api/payment/gateway/epay"
)

func TestPaymentDetailProjectionKeepsProductsAndCapabilities(t *testing.T) {
	for _, brokenCredentials := range []bool{false, true} {
		t.Run(map[bool]string{false: "ready", true: "broken_credentials"}[brokenCredentials], func(t *testing.T) {
			useOrderCallbackTestDB(t)
			p, _, _ := callbackFixture(t, false)
			if brokenCredentials {
				if err := model.DB.Model(p).Update("config", "invalid-json").Error; err != nil {
					t.Fatal(err)
				}
			}
			gin.SetMode(gin.TestMode)
			router := gin.New()
			router.GET("/api/payment/:id", GetPayment)
			router.GET("/api/payment", GetPaymentList)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/payment/7", nil))
			var response struct {
				Success bool                       `json:"success"`
				Data    map[string]json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if recorder.Code != http.StatusOK || !response.Success {
				t.Fatalf("详情响应失败: %s", recorder.Body.String())
			}
			var products []string
			if err := json.Unmarshal(response.Data["products"], &products); err != nil {
				t.Fatalf("产品列表缺失: %s", recorder.Body.String())
			}
			if len(products) != 1 || products[0] != epay.Checkout {
				t.Fatalf("产品列表错误: %v", products)
			}
			caps, exists := response.Data["capabilities"]
			if !exists || (!brokenCredentials && string(caps) == "null") || (brokenCredentials && string(caps) != "null") {
				t.Fatalf("能力字段错误: %s", recorder.Body.String())
			}
			var profile string
			if err := json.Unmarshal(response.Data["protocol_profile"], &profile); err != nil || profile != p.Identity.ProtocolProfile {
				t.Fatalf("协议投影错误: %s", recorder.Body.String())
			}
			if string(response.Data["id"]) != "7" || response.Data["identity"] == nil || response.Data["config"] == nil {
				t.Fatalf("网关字段丢失: %s", recorder.Body.String())
			}
			recorder = httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/payment", nil))
			var list struct {
				Data struct {
					Data  []map[string]json.RawMessage `json:"data"`
					Total int                          `json:"total_count"`
				} `json:"data"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &list); err != nil {
				t.Fatal(err)
			}
			if len(list.Data.Data) != 1 || list.Data.Total != 1 || string(list.Data.Data[0]["protocol_profile"]) != `"`+epay.Profile+`"` {
				t.Fatalf("列表协议字段或分页丢失: %s", recorder.Body.String())
			}
		})
	}
}
