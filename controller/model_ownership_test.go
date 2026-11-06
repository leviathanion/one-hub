package controller

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"one-api/model"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func setupModelOwnershipControllerTest(t *testing.T) *gin.Engine {
	t.Helper()
	router := setupPricingControllerTest(t)
	if err := model.DB.AutoMigrate(&model.ModelOwnedBy{}, &model.Channel{}); err != nil {
		t.Fatal(err)
	}
	if err := model.DB.Create(&model.ModelOwnedBy{Id: 1001, Name: "自定义厂商"}).Error; err != nil {
		t.Fatal(err)
	}
	router.GET("/ownership/catalog", GetModelOwnershipCatalog)
	router.POST("/ownership/suggestions", GetModelOwnershipSuggestions)
	router.POST("/ownership/batch", BatchSetModelOwnership)
	return router
}

func TestModelOwnershipImportUpdatesDisplayWithoutPublishingPrices(t *testing.T) {
	router := setupModelOwnershipControllerTest(t)
	price := &model.Price{Model: "catalog-model", Type: "tokens", Input: 2, Output: 3}
	if err := model.PricingInstance.AddPrice(price); err != nil {
		t.Fatal(err)
	}
	_, beforeVersion := model.PricingInstance.GetAllPricesListWithVersion()
	if err := model.DB.Create(&model.ModelInfo{Model: "existing", Description: "保留描述", ContextLength: 12345}).Error; err != nil {
		t.Fatal(err)
	}
	old := http.DefaultTransport
	http.DefaultTransport = modelsDevTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != model.ModelsDevURL {
			t.Fatalf("unexpected source %s", req.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"host":{"models":{"catalog-model":{"canonical_model_id":"lab/catalog-model"}}}}`))}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = old })
	response := performPricingRequest(t, router, "/ownership/suggestions", `{"models":["catalog-model","existing"]}`)
	var suggestions struct {
		Success bool                             `json:"success"`
		Data    []model.ModelOwnershipSuggestion `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &suggestions); err != nil || !suggestions.Success || response.Code != http.StatusOK {
		t.Fatalf("suggestions failed: %s (%v)", response.Body.String(), err)
	}
	if len(suggestions.Data) != 2 || suggestions.Data[0].Status != "matched" || suggestions.Data[0].Labs[0] != "lab" || suggestions.Data[1].Status != "unmatched" {
		t.Fatalf("unexpected suggestions: %+v", suggestions.Data)
	}
	var count int64
	if err := model.DB.Model(&model.ModelInfo{}).Where("model = ?", price.Model).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("preview created catalog rows: %d %v", count, err)
	}
	response = performPricingRequest(t, router, "/ownership/batch", `{"mode":"fill_missing","items":[{"model":"catalog-model","owned_by_id":1001},{"model":"existing","owned_by_id":1001}]}`)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"created":1`) || !strings.Contains(response.Body.String(), `"updated":1`) {
		t.Fatalf("batch failed: %d %s", response.Code, response.Body.String())
	}
	info, err := model.GetModelInfoByModel("existing")
	if err != nil || info.Description != "保留描述" || info.ContextLength != 12345 {
		t.Fatalf("existing metadata changed: %+v %v", info, err)
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/prices", nil))
	var prices struct {
		Success bool           `json:"success"`
		Version int64          `json:"version"`
		Data    []*model.Price `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &prices); err != nil || !prices.Success || len(prices.Data) != 1 {
		t.Fatalf("price display failed: %s %v", recorder.Body.String(), err)
	}
	got := prices.Data[0]
	if prices.Version != beforeVersion || got.Input != price.Input || got.Output != price.Output || got.ModelInfo == nil || got.ModelInfo.OwnedByID == nil || *got.ModelInfo.OwnedByID != 1001 {
		t.Fatalf("ownership must refresh display without publishing prices: %+v version %d -> %d", got, beforeVersion, prices.Version)
	}
}

func TestModelOwnershipHTTPRejectsInvalidWritesWithoutChangingCatalog(t *testing.T) {
	router := setupModelOwnershipControllerTest(t)
	for _, body := range []string{
		`null`,
		`{"mode":"replace","items":[{"model":"new"}]}`,
		`{"mode":"replace","items":[{"model":"new","owned_by_id":9999}]}`,
		`{"mode":"replace","items":[{"model":"new","owned_by_id":1001,"description":"unexpected"}]}`,
		`{"mode":"replace","items":[{"model":"new","owned_by_id":1001}]} {}`,
		`{"mode":"fill_missing","items":[{"model":"new","owned_by_id":null}]}`,
	} {
		response := performPricingRequest(t, router, "/ownership/batch", body)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid batch status=%d body=%s", response.Code, response.Body.String())
		}
	}
	response := performPricingRequest(t, router, "/ownership/batch", `{"mode":"replace","items":[],"padding":"`+strings.Repeat("x", maxModelOwnershipRequestBytes)+`"}`)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized request status=%d", response.Code)
	}
	var count int64
	if err := model.DB.Model(&model.ModelInfo{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("invalid writes created %d rows: %v", count, err)
	}
}

func TestModelOwnershipSuggestionsValidateBeforeFetchingAndReportSourceFailure(t *testing.T) {
	router := setupModelOwnershipControllerTest(t)
	old := http.DefaultTransport
	calls := 0
	http.DefaultTransport = modelsDevTransport(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("source unavailable")
	})
	t.Cleanup(func() { http.DefaultTransport = old })
	for _, body := range []string{`{"models":[]}`, `{"models":["family-*"]}`, `{"models":["valid"],"url":"https://unexpected.example"}`} {
		response := performPricingRequest(t, router, "/ownership/suggestions", body)
		if response.Code != http.StatusBadRequest || calls != 0 {
			t.Fatalf("invalid input reached source: %d calls=%d %s", response.Code, calls, response.Body.String())
		}
	}
	response := performPricingRequest(t, router, "/ownership/suggestions", `{"models":["valid"]}`)
	if response.Code != http.StatusBadGateway || calls != 1 {
		t.Fatalf("source failure disguised as empty match: %d calls=%d %s", response.Code, calls, response.Body.String())
	}
}
