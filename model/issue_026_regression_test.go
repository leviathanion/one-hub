package model

import (
	"testing"

	"one-api/common/config"

	"gorm.io/datatypes"
)

func issue026DefaultWhisperPrice(t *testing.T) *Price {
	t.Helper()
	for _, price := range GetDefaultPrice() {
		if price.Model == "whisper-1" {
			return price
		}
	}
	t.Fatal("whisper-1 default price is missing")
	return nil
}

func TestI026WhisperDefaultUsesDurationPriceContainer(t *testing.T) {
	price := issue026DefaultWhisperPrice(t)
	if price.Type != TokensPriceType || price.ChannelType != config.ChannelTypeOpenAI || price.Input != 50 || price.Output != 0 {
		t.Fatalf("unexpected whisper default price: %+v", price)
	}
	if price.ExtraRatios == nil {
		t.Fatal("whisper default price is missing duration ratio")
	}
	ratio, present := price.ExtraRatios.Data()[config.UsageExtraInputAudioTranscription]
	if !present || ratio != 1 {
		t.Fatalf("whisper duration ratio=%v present=%t want=1", ratio, present)
	}
}

func TestI026SystemPriceAddPreservesExistingWhisperAndCustomRows(t *testing.T) {
	db := openPriceSyncTestDB(t)
	extra := datatypes.NewJSONType(map[string]float64{config.UsageExtraInputAudioTranscription: 2})
	seeded := []*Price{
		{Model: "whisper-1", Type: TokensPriceType, ChannelType: config.ChannelTypeOpenAI, Input: 15, Output: 15},
		{Model: "whisper-custom", Type: TokensPriceType, ChannelType: config.ChannelTypeOpenAI, Input: 7, Output: 8, ExtraRatios: &extra},
		{Model: "whisper-locked", Type: TokensPriceType, ChannelType: config.ChannelTypeOpenAI, Input: 9, Output: 10, Locked: true},
		{Model: "whisper-times", Type: TimesPriceType, ChannelType: config.ChannelTypeOpenAI, Input: 1, Output: 0},
	}
	if err := db.Create(seeded).Error; err != nil {
		t.Fatal(err)
	}
	pricing := &Pricing{Prices: make(map[string]*Price)}
	if err := pricing.Init(); err != nil {
		t.Fatal(err)
	}
	if err := pricing.SyncPricing(GetDefaultPrice(), string(PriceUpdateModeSystem)); err != nil {
		t.Fatal(err)
	}

	var whisper, custom, locked, times Price
	for name, target := range map[string]*Price{
		"whisper-1":      &whisper,
		"whisper-custom": &custom,
		"whisper-locked": &locked,
		"whisper-times":  &times,
	} {
		if err := db.Where("model = ?", name).First(target).Error; err != nil {
			t.Fatal(err)
		}
	}
	if whisper.Type != TokensPriceType || whisper.Input != 15 || whisper.Output != 15 || whisper.ExtraRatios != nil {
		t.Fatalf("system add overwrote existing whisper row: %+v", whisper)
	}
	if custom.Input != 7 || custom.Output != 8 || custom.ExtraRatios == nil || custom.ExtraRatios.Data()[config.UsageExtraInputAudioTranscription] != 2 {
		t.Fatalf("system add changed custom whisper row: %+v", custom)
	}
	if !locked.Locked || locked.Input != 9 || locked.Output != 10 {
		t.Fatalf("system add changed locked row: %+v", locked)
	}
	if times.Type != TimesPriceType || times.Input != 1 || times.Output != 0 || times.ExtraRatios != nil {
		t.Fatalf("system add changed times row: %+v", times)
	}

	restarted := &Pricing{Prices: make(map[string]*Price)}
	if err := restarted.Init(); err != nil {
		t.Fatal(err)
	}
	if err := restarted.SyncPricing(GetDefaultPrice(), string(PriceUpdateModeSystem)); err != nil {
		t.Fatal(err)
	}
	var afterRestart Price
	if err := db.Where("model = ?", "whisper-1").First(&afterRestart).Error; err != nil {
		t.Fatal(err)
	}
	if afterRestart.Input != 15 || afterRestart.Output != 15 || afterRestart.ExtraRatios != nil {
		t.Fatalf("system restart overwrote existing whisper row: %+v", afterRestart)
	}
}
