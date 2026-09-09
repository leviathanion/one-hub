package controller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"one-api/internal/testutil/sqlitetest"
	"one-api/model"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func decodeUpdatePriceRequestForTest(t *testing.T, body string) (*updatePriceRequest, bool) {
	t.Helper()
	var request updatePriceRequest
	if err := json.Unmarshal([]byte(body), &request); err != nil {
		t.Fatalf("decode update price request: %v", err)
	}
	_, present, err := request.price()
	if err != nil {
		t.Fatalf("convert update price request: %v", err)
	}
	return &request, present
}

func TestUpdatePriceRequestTracksRateRulesPresence(t *testing.T) {
	omitted, present := decodeUpdatePriceRequestForTest(t, `{"expected_version":1,"model":"gpt-5","type":"tokens","channel_type":1,"input":1,"output":2}`)
	if present || len(omitted.RateRules) != 0 {
		t.Fatalf("expected omitted rate_rules to remain absent, request=%+v present=%v", omitted, present)
	}

	empty, present := decodeUpdatePriceRequestForTest(t, `{"expected_version":1,"model":"gpt-5","type":"tokens","channel_type":1,"input":1,"output":2,"rate_rules":{}}`)
	price, _, err := empty.price()
	if err != nil || !present || price.RateRules == nil || testRuleMultiplier(price.EffectiveRateRules(), "flex") != nil {
		t.Fatalf("expected explicit empty object to clear rules, price=%+v present=%v err=%v", price, present, err)
	}

	configured, present := decodeUpdatePriceRequestForTest(t, `{"expected_version":1,"model":"gpt-5","type":"tokens","channel_type":1,"input":1,"output":2,"rate_rules":{"version":2,"service_tier":[{"id":"flex","when":{"service_tier":["flex"]},"multipliers":{"input":0.5,"output":0.5}}]}}`)
	price, _, err = configured.price()
	if err != nil || !present || testRuleMultiplier(price.EffectiveRateRules(), "flex") == nil || *testRuleMultiplier(price.EffectiveRateRules(), "flex").Input != 0.5 {
		t.Fatalf("expected explicit rules to replace the stored policy, price=%+v present=%v err=%v", price, present, err)
	}
}

func TestUpdatePriceRequestRejectsNullRateRules(t *testing.T) {
	var request updatePriceRequest
	if err := json.Unmarshal([]byte(`{"expected_version":1,"model":"gpt-5","type":"tokens","channel_type":1,"input":1,"output":2,"rate_rules":null}`), &request); err != nil {
		t.Fatalf("decode update price request: %v", err)
	}
	if _, present, err := request.price(); err == nil || !present {
		t.Fatalf("expected explicit null rate_rules to be rejected, present=%v err=%v", present, err)
	}
}

func setupPricingControllerTest(t *testing.T) *gin.Engine {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.Price{}, &model.ModelInfo{}, &model.PublicationVersion{}); err != nil {
		t.Fatal(err)
	}
	if err := model.EnsurePublicationVersionRows(db); err != nil {
		t.Fatal(err)
	}
	originalDB := model.DB
	originalPricing := model.PricingInstance
	model.DB = db
	model.PricingInstance = &model.Pricing{Prices: make(map[string]*model.Price)}
	if err := model.PricingInstance.Init(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		model.DB = originalDB
		model.PricingInstance = originalPricing
	})
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/preview", PreviewPriceChange)
	router.POST("/apply", ApplyPriceChange)
	router.POST("/batch", BatchSetPrices)
	router.GET("/prices", GetPricesList)
	return router
}

func TestBatchPriceRequiresInputAndOutputButAcceptsExplicitZero(t *testing.T) {
	router := setupPricingControllerTest(t)
	missing := performPricingRequest(t, router, "/batch", `{"original_models":[],"models":["free"],"expected_version":1,"price":{"type":"tokens","channel_type":0,"output":0,"locked":false}}`)
	var missingEnvelope struct {
		Success bool `json:"success"`
	}
	if err := json.Unmarshal(missing.Body.Bytes(), &missingEnvelope); err != nil {
		t.Fatal(err)
	}
	if missingEnvelope.Success {
		t.Fatalf("batch price accepted missing input: %s", missing.Body.String())
	}

	explicitZero := performPricingRequest(t, router, "/batch", `{"original_models":[],"models":["free"],"expected_version":1,"price":{"type":"tokens","channel_type":0,"input":0,"output":0,"locked":false}}`)
	var zeroEnvelope struct {
		Success bool `json:"success"`
	}
	if err := json.Unmarshal(explicitZero.Body.Bytes(), &zeroEnvelope); err != nil {
		t.Fatal(err)
	}
	if !zeroEnvelope.Success {
		t.Fatalf("batch price rejected explicit zero: %s", explicitZero.Body.String())
	}
	if price, ok := model.PricingInstance.FindExactPrice("free"); !ok || price.Input != 0 || price.Output != 0 {
		t.Fatalf("explicit zero price was not published: %+v ok=%v", price, ok)
	}
}

func TestGetPricesReturnsHealthyEmptyCatalogWithVersion(t *testing.T) {
	router := setupPricingControllerTest(t)
	request := httptest.NewRequest(http.MethodGet, "/prices", nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Success bool          `json:"success"`
		Version int64         `json:"version"`
		Data    []model.Price `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.Success || payload.Version != 1 || len(payload.Data) != 0 {
		t.Fatalf("unexpected empty catalog response: %+v", payload)
	}
}

func performPricingRequest(t *testing.T, router http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func TestPriceChangePreviewAndApplyHTTPContract(t *testing.T) {
	router := setupPricingControllerTest(t)
	previewBody := `{"mode":"overwrite","source":[{"model":"free","type":"tokens","input":0,"output":0,"rate_rules":{}}]}`
	previewResponse := performPricingRequest(t, router, "/preview", previewBody)
	if previewResponse.Code != http.StatusOK {
		t.Fatalf("preview status=%d body=%s", previewResponse.Code, previewResponse.Body.String())
	}
	var previewEnvelope struct {
		Success bool `json:"success"`
		Data    struct {
			BaseVersion int64                 `json:"base_version"`
			Digest      string                `json:"digest"`
			Plan        model.PriceChangePlan `json:"plan"`
		} `json:"data"`
	}
	if err := json.Unmarshal(previewResponse.Body.Bytes(), &previewEnvelope); err != nil {
		t.Fatal(err)
	}
	if !previewEnvelope.Success || previewEnvelope.Data.BaseVersion != 1 || len(previewEnvelope.Data.Plan.Changes) != 1 {
		t.Fatalf("unexpected preview: %+v", previewEnvelope)
	}
	applyBody, err := json.Marshal(map[string]any{
		"mode":         "overwrite",
		"source":       []map[string]any{{"model": "free", "type": "tokens", "input": 0, "output": 0, "rate_rules": map[string]any{}}},
		"base_version": previewEnvelope.Data.BaseVersion,
		"digest":       previewEnvelope.Data.Digest,
	})
	if err != nil {
		t.Fatal(err)
	}
	applyResponse := performPricingRequest(t, router, "/apply", string(applyBody))
	if applyResponse.Code != http.StatusOK {
		t.Fatalf("apply status=%d body=%s", applyResponse.Code, applyResponse.Body.String())
	}
	if _, ok := model.PricingInstance.FindExactPrice("free"); !ok || model.PricingInstance.PublishedVersion() != 2 {
		t.Fatal("applied change was not published")
	}
}

func TestPriceChangeApplyReturnsConflictForStalePreview(t *testing.T) {
	router := setupPricingControllerTest(t)
	body := `{"mode":"add","source":[{"model":"new","type":"tokens","input":1,"output":2}]}`
	previewResponse := performPricingRequest(t, router, "/preview", body)
	var preview struct {
		Data struct {
			BaseVersion int64  `json:"base_version"`
			Digest      string `json:"digest"`
		} `json:"data"`
	}
	if err := json.Unmarshal(previewResponse.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	if _, err := model.BumpPublicationVersionCAS(t.Context(), model.DB, model.PublicationOwnerPrice, preview.Data.BaseVersion); err != nil {
		t.Fatal(err)
	}
	applyBody, err := json.Marshal(map[string]any{
		"mode":         "add",
		"source":       []map[string]any{{"model": "new", "type": "tokens", "input": 1, "output": 2}},
		"base_version": preview.Data.BaseVersion,
		"digest":       preview.Data.Digest,
	})
	if err != nil {
		t.Fatal(err)
	}
	response := performPricingRequest(t, router, "/apply", string(applyBody))
	if response.Code != http.StatusConflict {
		t.Fatalf("stale apply status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestPriceChangePreviewRejectsUnknownManualEnvelopeField(t *testing.T) {
	router := setupPricingControllerTest(t)
	response := performPricingRequest(t, router, "/preview", `{"mode":"add","source":[{"model":"new","type":"tokens","input":1,"output":2}],"ignored":true}`)
	if response.Code != http.StatusOK {
		t.Fatalf("unexpected status=%d", response.Code)
	}
	var envelope struct {
		Success bool `json:"success"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Success {
		t.Fatal("unknown preview field was accepted")
	}
}

func testRuleMultiplier(rules model.PriceRateRules, id string) *model.PriceRateMultiplier {
	for _, rule := range rules.ServiceTier {
		if rule.ID == id {
			return &rule.Multipliers
		}
	}
	return nil
}
