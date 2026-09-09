package controller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spf13/viper"

	"one-api/model"
)

func TestI020PublishedPriceCatalogWithModelInfoRoundTripsPreviewAndApply(t *testing.T) {
	router := setupPricingControllerTest(t)
	if err := model.CreateModelInfo(&model.ModelInfo{
		Model:            "peer-model",
		Name:             "Peer model",
		InputModalities:  `["text"]`,
		OutputModalities: `["text"]`,
	}); err != nil {
		t.Fatal(err)
	}
	if err := model.PricingInstance.AddPrice(&model.Price{
		Model:       "peer-model",
		Type:        model.TokensPriceType,
		ChannelType: 1,
		Input:       2,
		Output:      4,
	}); err != nil {
		t.Fatal(err)
	}

	publicRequest := httptest.NewRequest(http.MethodGet, "/prices", nil)
	publicResponse := httptest.NewRecorder()
	router.ServeHTTP(publicResponse, publicRequest)
	if publicResponse.Code != http.StatusOK {
		t.Fatalf("public prices status=%d body=%s", publicResponse.Code, publicResponse.Body.String())
	}
	var publicCatalog struct {
		Success bool            `json:"success"`
		Message string          `json:"message"`
		Version int64           `json:"version"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(publicResponse.Body.Bytes(), &publicCatalog); err != nil {
		t.Fatal(err)
	}
	if !publicCatalog.Success || publicCatalog.Version != 2 || publicCatalog.Message != "" {
		t.Fatalf("unexpected public price envelope: %+v", publicCatalog)
	}
	var publicRows []map[string]any
	if err := json.Unmarshal(publicCatalog.Data, &publicRows); err != nil || len(publicRows) != 1 {
		t.Fatalf("public price rows unavailable: err=%v rows=%+v", err, publicRows)
	}
	if _, ok := publicRows[0]["model_info"]; !ok {
		t.Fatalf("public price row lost model_info: %+v", publicRows[0])
	}
	publicSource := json.RawMessage(append([]byte(nil), publicResponse.Body.Bytes()...))

	// 记录公开源后修改本地价格，确保预览和应用都经过共享目录解析器。
	if err := model.PricingInstance.UpdatePriceAtVersion(
		"peer-model",
		&model.Price{Model: "peer-model", Type: model.TokensPriceType, ChannelType: 1, Input: 1, Output: 1},
		false,
		publicCatalog.Version,
	); err != nil {
		t.Fatal(err)
	}

	previewBody, err := json.Marshal(map[string]any{
		"mode":   string(model.PriceUpdateModeUpdate),
		"source": publicSource,
	})
	if err != nil {
		t.Fatal(err)
	}
	previewResponse := performPricingRequest(t, router, "/preview", string(previewBody))
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
	if !previewEnvelope.Success || previewEnvelope.Data.BaseVersion != 3 || len(previewEnvelope.Data.Plan.Changes) != 1 || previewEnvelope.Data.Plan.Changes[0].Action != model.PriceChangeUpdate {
		t.Fatalf("published catalog did not produce update preview: %+v", previewEnvelope)
	}

	applyBody, err := json.Marshal(map[string]any{
		"mode":         string(model.PriceUpdateModeUpdate),
		"source":       publicSource,
		"base_version": previewEnvelope.Data.BaseVersion,
		"digest":       previewEnvelope.Data.Digest,
	})
	if err != nil {
		t.Fatal(err)
	}
	applyRequest := httptest.NewRequest(http.MethodPost, "/apply", bytes.NewReader(applyBody))
	applyRequest.Header.Set("Content-Type", "application/json")
	applyResponse := httptest.NewRecorder()
	router.ServeHTTP(applyResponse, applyRequest)
	if applyResponse.Code != http.StatusOK {
		t.Fatalf("apply status=%d body=%s", applyResponse.Code, applyResponse.Body.String())
	}
	var applyEnvelope struct {
		Success bool  `json:"success"`
		Version int64 `json:"version"`
	}
	if err := json.Unmarshal(applyResponse.Body.Bytes(), &applyEnvelope); err != nil {
		t.Fatal(err)
	}
	if !applyEnvelope.Success || applyEnvelope.Version != 4 {
		t.Fatalf("published catalog apply did not commit version 4: %+v", applyEnvelope)
	}
	var stored model.Price
	if err := model.DB.Where("model = ?", "peer-model").First(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Input != 2 || stored.Output != 4 {
		t.Fatalf("public catalog values were not applied: %+v", stored)
	}
}

func captureI020PublishedArray(t *testing.T, router http.Handler) (json.RawMessage, int64) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/prices", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("public prices status=%d body=%s", response.Code, response.Body.String())
	}
	var envelope struct {
		Success bool            `json:"success"`
		Message string          `json:"message"`
		Version int64           `json:"version"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if !envelope.Success || envelope.Message != "" || envelope.Version < 1 || len(envelope.Data) == 0 {
		t.Fatalf("unexpected public price envelope: %+v", envelope)
	}
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(envelope.Data, &rows); err != nil || len(rows) != 2 {
		t.Fatalf("public price array unavailable: err=%v rows=%+v", err, rows)
	}
	for index, row := range rows {
		if _, ok := row["model_info"]; ok {
			t.Fatalf("public array row %d unexpectedly contains model_info: %+v", index, row)
		}
	}
	return json.RawMessage(append([]byte(nil), envelope.Data...)), envelope.Version
}

// setupI020PublishedArray 通过公开接口构造源目录，再恢复出三种同步模式所需的本地状态。
func setupI020PublishedArray(t *testing.T, router http.Handler, withStale bool) (json.RawMessage, int64) {
	t.Helper()
	publisher := model.PricingInstance
	if err := publisher.AddPrice(&model.Price{
		Model:       "published-existing",
		Type:        model.TokensPriceType,
		ChannelType: 1,
		Input:       1,
		Output:      1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := publisher.UpdatePriceAtVersion("published-existing", &model.Price{
		Model:       "published-existing",
		Type:        model.TokensPriceType,
		ChannelType: 1,
		Input:       2,
		Output:      3,
	}, false, 2); err != nil {
		t.Fatal(err)
	}
	if err := publisher.AddPrice(&model.Price{
		Model:       "published-new",
		Type:        model.TokensPriceType,
		ChannelType: 1,
		Input:       4,
		Output:      5,
	}); err != nil {
		t.Fatal(err)
	}
	source, publishedVersion := captureI020PublishedArray(t, router)
	if publishedVersion != 4 {
		t.Fatalf("unexpected source publication version %d", publishedVersion)
	}
	if err := publisher.UpdatePriceAtVersion("published-existing", &model.Price{
		Model:       "published-existing",
		Type:        model.TokensPriceType,
		ChannelType: 1,
		Input:       1,
		Output:      1,
	}, false, 4); err != nil {
		t.Fatal(err)
	}
	if err := publisher.DeletePriceAtVersion("published-new", 5); err != nil {
		t.Fatal(err)
	}
	localVersion := int64(6)
	if withStale {
		if err := publisher.AddPrice(&model.Price{
			Model:       "published-stale",
			Type:        model.TokensPriceType,
			ChannelType: 1,
			Input:       9,
			Output:      9,
		}); err != nil {
			t.Fatal(err)
		}
		localVersion++
	}
	if version := publisher.PublishedVersion(); version != localVersion {
		t.Fatalf("local publication version=%d want=%d", version, localVersion)
	}
	return source, localVersion
}

func TestI020PublishedArrayWithoutModelInfoDrivesPreviewAndApply(t *testing.T) {
	router := setupPricingControllerTest(t)
	source, baseVersion := setupI020PublishedArray(t, router, false)
	previewBody, err := json.Marshal(map[string]any{
		"mode":   string(model.PriceUpdateModeUpdate),
		"source": source,
	})
	if err != nil {
		t.Fatal(err)
	}
	previewResponse := performPricingRequest(t, router, "/preview", string(previewBody))
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
	if !previewEnvelope.Success || previewEnvelope.Data.BaseVersion != baseVersion || len(previewEnvelope.Data.Plan.Changes) != 1 || previewEnvelope.Data.Plan.Changes[0].Model != "published-existing" {
		t.Fatalf("published array did not produce the expected update preview: %+v", previewEnvelope)
	}

	applyBody, err := json.Marshal(map[string]any{
		"mode":         string(model.PriceUpdateModeUpdate),
		"source":       source,
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
	var applyEnvelope struct {
		Success bool  `json:"success"`
		Version int64 `json:"version"`
	}
	if err := json.Unmarshal(applyResponse.Body.Bytes(), &applyEnvelope); err != nil {
		t.Fatal(err)
	}
	if !applyEnvelope.Success || applyEnvelope.Version != baseVersion+1 {
		t.Fatalf("published array apply did not commit expected version: %+v", applyEnvelope)
	}
	var existing model.Price
	if err := model.DB.Where("model = ?", "published-existing").First(&existing).Error; err != nil {
		t.Fatal(err)
	}
	if existing.Input != 2 || existing.Output != 3 {
		t.Fatalf("published array values were not applied: %+v", existing)
	}
	var newCount int64
	if err := model.DB.Model(&model.Price{}).Where("model = ?", "published-new").Count(&newCount).Error; err != nil {
		t.Fatal(err)
	}
	if newCount != 0 || model.PricingInstance.PublishedVersion() != baseVersion+1 {
		t.Fatalf("update mode changed an absent model or local version unexpectedly: count=%d version=%d", newCount, model.PricingInstance.PublishedVersion())
	}
}

func TestI020PublishedArrayWithoutModelInfoDrivesAutomaticModes(t *testing.T) {
	for _, mode := range []model.PriceUpdateMode{model.PriceUpdateModeAdd, model.PriceUpdateModeUpdate, model.PriceUpdateModeOverwrite} {
		t.Run(string(mode), func(t *testing.T) {
			router := setupPricingControllerTest(t)
			source, baseVersion := setupI020PublishedArray(t, router, mode == model.PriceUpdateModeOverwrite)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(source)
			}))
			t.Cleanup(server.Close)
			oldURL := viper.GetString("update_price_service")
			oldMode := viper.GetString("auto_price_updates_mode")
			viper.Set("update_price_service", server.URL)
			viper.Set("auto_price_updates_mode", string(mode))
			t.Cleanup(func() {
				viper.Set("update_price_service", oldURL)
				viper.Set("auto_price_updates_mode", oldMode)
			})

			if err := model.UpdatePriceByPriceService(); err != nil {
				t.Fatalf("auto %s sync rejected the actual public array: %v", mode, err)
			}
			if version := model.PricingInstance.PublishedVersion(); version != baseVersion+1 {
				t.Fatalf("auto %s sync published unexpected local version %d", mode, version)
			}
			var existing model.Price
			if err := model.DB.Where("model = ?", "published-existing").First(&existing).Error; err != nil {
				t.Fatal(err)
			}
			wantExistingInput, wantExistingOutput := 2.0, 3.0
			if mode == model.PriceUpdateModeAdd {
				wantExistingInput, wantExistingOutput = 1, 1
			}
			if existing.Input != wantExistingInput || existing.Output != wantExistingOutput {
				t.Fatalf("auto %s sync changed existing policy incorrectly: %+v", mode, existing)
			}

			var added model.Price
			newErr := model.DB.Where("model = ?", "published-new").First(&added).Error
			if mode == model.PriceUpdateModeUpdate {
				if newErr == nil {
					t.Fatalf("update mode inserted a source-only model: %+v", added)
				}
			} else {
				if newErr != nil {
					t.Fatalf("auto %s sync did not insert source model: %v", mode, newErr)
				}
				if added.Input != 4 || added.Output != 5 {
					t.Fatalf("auto %s sync inserted wrong policy: %+v", mode, added)
				}
			}
			if mode == model.PriceUpdateModeOverwrite {
				var staleCount int64
				if err := model.DB.Model(&model.Price{}).Where("model = ?", "published-stale").Count(&staleCount).Error; err != nil {
					t.Fatal(err)
				}
				if staleCount != 0 {
					t.Fatalf("overwrite mode retained stale model: count=%d", staleCount)
				}
			}
		})
	}
}
