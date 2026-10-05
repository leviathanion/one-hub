package controller

import (
	"encoding/json"
	"one-api/model"
	"testing"
)

func TestPriceSyncDuplicateCatalogExplainsSourceWithoutWriting(t *testing.T) {
	router := setupPricingControllerTest(t)
	for _, endpoint := range []string{"/preview", "/apply"} {
		body := `{"mode":"add","source":[{"model":"same","type":"tokens","input":1,"output":2},{"model":"same","type":"tokens","input":3,"output":4}]`
		if endpoint == "/apply" {
			body += `,"base_version":1,"digest":"unused"`
		}
		response := performPricingRequest(t, router, endpoint, body+`}`)
		var result struct {
			Success bool
			Code    string
			Model   string
			Message string
		}
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.Success || result.Code != "duplicate_price_model" || result.Model != "same" || result.Message != `duplicate remote price model "same"` {
			t.Fatal(response.Body.String())
		}
	}
	var count int64
	if err := model.DB.Model(&model.Price{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("invalid catalog wrote prices: %d %v", count, err)
	}
	response := performPricingRequest(t, router, "/preview", `{"mode":"add","source":[{"model":"invalid","type":"tokens","input":1}]}`)
	var result struct{ Code string }
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.Code != "invalid_price_catalog" {
		t.Fatal(response.Body.String(), err)
	}
}
