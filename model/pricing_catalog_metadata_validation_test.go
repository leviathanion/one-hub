package model

import (
	"context"
	"testing"
)

func TestRemotePriceCatalogAcceptsPublishedMetadata(t *testing.T) {
	const row = `{"model":"catalog-model","type":"tokens","input":0,"output":2,"locked":false,"extra_ratios":{"cache":0.5},"rate_rules":{},"model_info":{"model":"catalog-model","future_description":{"owner":"internal"},"input_modalities":["text"]}}`
	for _, payload := range []string{
		`[` + row + `]`,
		`{"success":true,"message":"","version":42,"data":[` + row + `]}`,
		`{"success":true,"message":"published","version":42,"data":[{"model":"catalog-model","type":"tokens","input":0,"output":2,"locked":false,"model_info":null}]}`,
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

func TestRemotePriceCatalogKeepsPricingValidationClosed(t *testing.T) {
	for name, payload := range map[string]string{
		"removed channel type":  `[ {"model":"m","type":"tokens","input":1,"output":2,"channel_type":1} ]`,
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

func TestRemotePriceCatalogKeepsSingleValueAndBoundedInputRules(t *testing.T) {
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

func TestPriceChangeDigestIgnoresReadOnlyModelMetadata(t *testing.T) {
	setupVersionedPricingTest(t)
	var digest string
	for _, info := range []string{`null`, `{"owned_by_id":1,"future":[1,2]}`, `{"owned_by_id":1001,"name":"new name"}`} {
		prices, err := DecodeRemotePriceCatalog([]byte(`[{"model":"catalog-model","type":"tokens","input":1,"output":2,"model_info":` + info + `}]`))
		if err != nil {
			t.Fatal(err)
		}
		preview, err := PreviewPriceChange(context.Background(), prices, PriceUpdateModeAdd)
		if err != nil {
			t.Fatal(err)
		}
		if digest != "" && preview.Digest != digest {
			t.Fatal("read-only metadata changed price digest")
		}
		digest = preview.Digest
	}
}
