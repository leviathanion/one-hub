package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"one-api/common/config"
	"one-api/model"

	"gorm.io/datatypes"
)

func TestI026WhisperSystemAddAndTargetedCASPublish(t *testing.T) {
	router := setupPricingControllerTest(t)
	extra := datatypes.NewJSONType(map[string]float64{config.UsageExtraInputAudioTranscription: 2})
	seeded := []*model.Price{
		{Model: "whisper-1", Type: model.TokensPriceType, ChannelType: config.ChannelTypeOpenAI, Input: 15, Output: 15},
		{Model: "whisper-custom", Type: model.TokensPriceType, ChannelType: config.ChannelTypeOpenAI, Input: 7, Output: 8, ExtraRatios: &extra},
		{Model: "whisper-locked", Type: model.TokensPriceType, ChannelType: config.ChannelTypeOpenAI, Input: 9, Output: 10, Locked: true},
		{Model: "whisper-times", Type: model.TimesPriceType, ChannelType: config.ChannelTypeOpenAI, Input: 1, Output: 0},
		{Model: "i026-keep", Type: model.TokensPriceType, ChannelType: config.ChannelTypeOpenAI, Input: 3, Output: 4},
	}
	if err := model.DB.Create(seeded).Error; err != nil {
		t.Fatal(err)
	}
	if err := model.PricingInstance.Init(); err != nil {
		t.Fatal(err)
	}
	if err := model.PricingInstance.SyncPricing(model.GetDefaultPrice(), string(model.PriceUpdateModeSystem)); err != nil {
		t.Fatal(err)
	}
	if got := model.PricingInstance.PublishedVersion(); got != 2 {
		t.Fatalf("system add publication version=%d want=2", got)
	}

	assertI026StoredPrice := func(modelName string, want func(*model.Price) bool) {
		t.Helper()
		var price model.Price
		if err := model.DB.Where("model = ?", modelName).First(&price).Error; err != nil {
			t.Fatal(err)
		}
		if !want(&price) {
			t.Fatalf("stored price %q changed unexpectedly: %+v", modelName, price)
		}
	}
	assertI026StoredPrice("whisper-1", func(price *model.Price) bool {
		return price.Type == model.TokensPriceType && price.Input == 15 && price.Output == 15 && price.ExtraRatios == nil
	})
	assertI026StoredPrice("whisper-custom", func(price *model.Price) bool {
		return price.Input == 7 && price.Output == 8 && price.ExtraRatios != nil && price.ExtraRatios.Data()[config.UsageExtraInputAudioTranscription] == 2
	})
	assertI026StoredPrice("whisper-locked", func(price *model.Price) bool {
		return price.Locked && price.Input == 9 && price.Output == 10
	})
	assertI026StoredPrice("whisper-times", func(price *model.Price) bool {
		return price.Type == model.TimesPriceType && price.Input == 1 && price.Output == 0 && price.ExtraRatios == nil
	})

	// Simulate a restart before the approved target update. System mode must
	// remain add-only and leave the old persisted whisper row untouched.
	restarted := &model.Pricing{Prices: make(map[string]*model.Price)}
	model.PricingInstance = restarted
	if err := restarted.Init(); err != nil {
		t.Fatal(err)
	}
	if err := restarted.SyncPricing(model.GetDefaultPrice(), string(model.PriceUpdateModeSystem)); err != nil {
		t.Fatal(err)
	}
	if got := restarted.PublishedVersion(); got != 2 {
		t.Fatalf("restart system add changed publication version to %d", got)
	}
	assertI026StoredPrice("whisper-1", func(price *model.Price) bool {
		return price.Input == 15 && price.Output == 15 && price.ExtraRatios == nil
	})

	source := []map[string]any{{
		"model":        "whisper-1",
		"type":         model.TokensPriceType,
		"channel_type": config.ChannelTypeOpenAI,
		"input":        50,
		"output":       0,
		"locked":       false,
		"extra_ratios": map[string]float64{config.UsageExtraInputAudioTranscription: 1},
	}}
	previewBody, err := json.Marshal(map[string]any{
		"mode":   string(model.PriceUpdateModeUpdate),
		"source": source,
	})
	if err != nil {
		t.Fatal(err)
	}
	preview := performPricingRequest(t, router, "/preview", string(previewBody))
	if preview.Code != http.StatusOK {
		t.Fatalf("whisper targeted preview status=%d body=%s", preview.Code, preview.Body.String())
	}
	var previewEnvelope struct {
		Success bool `json:"success"`
		Data    struct {
			BaseVersion int64                 `json:"base_version"`
			Digest      string                `json:"digest"`
			Plan        model.PriceChangePlan `json:"plan"`
		} `json:"data"`
	}
	if err := json.Unmarshal(preview.Body.Bytes(), &previewEnvelope); err != nil {
		t.Fatal(err)
	}
	if !previewEnvelope.Success || previewEnvelope.Data.BaseVersion != 2 || previewEnvelope.Data.Digest == "" || len(previewEnvelope.Data.Plan.Changes) != 1 {
		t.Fatalf("unexpected whisper targeted preview: %+v", previewEnvelope)
	}
	change := previewEnvelope.Data.Plan.Changes[0]
	if change.Action != model.PriceChangeUpdate || change.Model != "whisper-1" {
		t.Fatalf("targeted preview changed the wrong model: %+v", change)
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
	apply := performPricingRequest(t, router, "/apply", string(applyBody))
	if apply.Code != http.StatusOK {
		t.Fatalf("whisper targeted apply status=%d body=%s", apply.Code, apply.Body.String())
	}
	var applyEnvelope struct {
		Success bool  `json:"success"`
		Version int64 `json:"version"`
	}
	if err := json.Unmarshal(apply.Body.Bytes(), &applyEnvelope); err != nil {
		t.Fatal(err)
	}
	if !applyEnvelope.Success || applyEnvelope.Version != 3 || restarted.PublishedVersion() != 3 {
		t.Fatalf("unexpected whisper targeted apply: %+v local=%d", applyEnvelope, restarted.PublishedVersion())
	}

	getRequest := httptest.NewRequest(http.MethodGet, "/prices?type=db", nil)
	getResponse := httptest.NewRecorder()
	router.ServeHTTP(getResponse, getRequest)
	if getResponse.Code != http.StatusOK {
		t.Fatalf("published price GET status=%d body=%s", getResponse.Code, getResponse.Body.String())
	}
	var published struct {
		Success bool          `json:"success"`
		Version int64         `json:"version"`
		Data    []model.Price `json:"data"`
	}
	if err := json.Unmarshal(getResponse.Body.Bytes(), &published); err != nil {
		t.Fatal(err)
	}
	if !published.Success || published.Version != 3 {
		t.Fatalf("unexpected published price response: %+v", published)
	}
	var target, keep model.Price
	for _, price := range published.Data {
		switch price.Model {
		case "whisper-1":
			target = price
		case "i026-keep":
			keep = price
		}
	}
	if target.Type != model.TokensPriceType || target.Input != 50 || target.Output != 0 || target.ExtraRatios == nil || target.ExtraRatios.Data()[config.UsageExtraInputAudioTranscription] != 1 {
		t.Fatalf("targeted whisper price was not published: %+v", target)
	}
	if keep.Input != 3 || keep.Output != 4 {
		t.Fatalf("non-target price changed during targeted publish: %+v", keep)
	}
	assertI026StoredPrice("whisper-locked", func(price *model.Price) bool {
		return price.Locked && price.Input == 9 && price.Output == 10
	})
	assertI026StoredPrice("whisper-times", func(price *model.Price) bool {
		return price.Type == model.TimesPriceType && price.Input == 1 && price.Output == 0 && price.ExtraRatios == nil
	})

	// The target update survives a second local reload; reapplying system mode
	// is a no-op because the approved target already matches the new default.
	if err := restarted.Init(); err != nil {
		t.Fatal(err)
	}
	if err := restarted.SyncPricing(model.GetDefaultPrice(), string(model.PriceUpdateModeSystem)); err != nil {
		t.Fatal(err)
	}
	if restarted.PublishedVersion() != 3 {
		t.Fatalf("post-publish system restart changed version to %d", restarted.PublishedVersion())
	}
	assertI026StoredPrice("whisper-1", func(price *model.Price) bool {
		return price.Input == 50 && price.Output == 0 && price.ExtraRatios != nil && price.ExtraRatios.Data()[config.UsageExtraInputAudioTranscription] == 1
	})
}
