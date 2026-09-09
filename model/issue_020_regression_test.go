package model

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spf13/viper"
)

func TestI020RemotePriceCatalogAcceptsPublishedMetadata(t *testing.T) {
	const row = `{"model":"catalog-model","type":"tokens","channel_type":1,"input":0,"output":2,"locked":false,"extra_ratios":{"cache":0.5},"rate_rules":{},"model_info":{"model":"catalog-model","future_description":{"owner":"internal"},"input_modalities":["text"]}}`
	for _, payload := range []string{
		`[` + row + `]`,
		`{"success":true,"message":"","version":42,"data":[` + row + `]}`,
		`{"success":true,"message":"published","version":42,"data":[{"model":"catalog-model","type":"tokens","channel_type":1,"input":0,"output":2,"locked":false,"model_info":null}]}`,
	} {
		prices, err := DecodeRemotePriceCatalog([]byte(payload))
		if err != nil || len(prices) != 1 {
			t.Fatalf("published catalog metadata was rejected: err=%v prices=%+v payload=%s", err, prices, payload)
		}
		if prices[0].Model != "catalog-model" || prices[0].Input != 0 || prices[0].Output != 2 || prices[0].ModelInfo != nil {
			t.Fatalf("metadata changed the pricing policy: %+v", prices[0])
		}
	}
}

func TestI020RemotePriceCatalogKeepsPricingValidationClosed(t *testing.T) {
	for name, payload := range map[string]string{
		"unknown row field":     `[ {"model":"m","type":"tokens","input":1,"output":2,"future_price":3} ]`,
		"unknown wrapper field": `{"success":true,"message":"","version":1,"data":[{"model":"m","type":"tokens","input":1,"output":2}],"future":true}`,
		"unknown rate rule":     `[ {"model":"m","type":"tokens","input":1,"output":2,"rate_rules":{"future":{"input":1,"output":1}}} ]`,
		"negative input":        `[ {"model":"m","type":"tokens","input":-1,"output":2} ]`,
		"missing input":         `[ {"model":"m","type":"tokens","output":2} ]`,
		"duplicate model":       `[ {"model":"m","type":"tokens","input":1,"output":2},{"model":"m","type":"tokens","input":3,"output":4} ]`,
		"null pricing field":    `[ {"model":"m","type":"tokens","input":null,"output":2} ]`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeRemotePriceCatalog([]byte(payload)); err == nil {
				t.Fatalf("invalid pricing contract was accepted: %s", payload)
			}
		})
	}
}

func TestI020RemotePriceCatalogKeepsSingleValueAndBoundedInputRules(t *testing.T) {
	for name, payload := range map[string]string{
		"trailing json": `[{"model":"m","type":"tokens","input":1,"output":2}] {"model":"n"}`,
		"empty array":   `[]`,
		"null data":     `{"success":true,"message":"","version":1,"data":null}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeRemotePriceCatalog([]byte(payload)); err == nil {
				t.Fatalf("invalid catalog envelope was accepted: %s", payload)
			}
		})
	}
}

func TestI020AutoPriceSyncKeepsDatabaseUnchangedForInvalidPublishedCatalog(t *testing.T) {
	db, pricing := setupVersionedPricingTest(t)
	stored := &Price{Model: "existing", Type: TokensPriceType, Input: 1, Output: 1}
	if err := db.Create(stored).Error; err != nil {
		t.Fatal(err)
	}
	if err := pricing.Init(); err != nil {
		t.Fatal(err)
	}
	originalPricing := PricingInstance
	PricingInstance = pricing
	t.Cleanup(func() { PricingInstance = originalPricing })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"message":"published","version":77,"data":[{"model":"existing","type":"tokens","input":-1,"output":2,"model_info":null}]}`))
	}))
	t.Cleanup(server.Close)
	oldURL := viper.GetString("update_price_service")
	oldMode := viper.GetString("auto_price_updates_mode")
	viper.Set("update_price_service", server.URL)
	viper.Set("auto_price_updates_mode", string(PriceUpdateModeUpdate))
	t.Cleanup(func() {
		viper.Set("update_price_service", oldURL)
		viper.Set("auto_price_updates_mode", oldMode)
	})
	if err := UpdatePriceByPriceService(); err == nil {
		t.Fatal("invalid remote price was accepted")
	}
	if version := pricing.PublishedVersion(); version != 1 {
		t.Fatalf("invalid remote price advanced version to %d", version)
	}
	var unchanged Price
	if err := db.Where("model = ?", "existing").First(&unchanged).Error; err != nil {
		t.Fatal(err)
	}
	if unchanged.Input != 1 || unchanged.Output != 1 {
		t.Fatalf("invalid remote price mutated local policy: %+v", unchanged)
	}
}
