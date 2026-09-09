package model

import "testing"

func TestDecodeRemotePriceCatalogIsStrictAndPresenceAware(t *testing.T) {
	valid, err := decodeRemotePriceCatalog([]byte(`{"data":[{"model":"free-model","type":"tokens","input":0,"output":0,"rate_rules":{}}]}`))
	if err != nil || len(valid) != 1 || valid[0].Input != 0 || valid[0].Output != 0 || valid[0].RateRules == nil {
		t.Fatalf("explicit zero remote price rejected: prices=%+v err=%v", valid, err)
	}
	for name, payload := range map[string]string{
		"null item":       `{"data":[null]}`,
		"missing model":   `{"data":[{"type":"tokens","input":1,"output":2}]}`,
		"missing input":   `{"data":[{"model":"m","type":"tokens","output":2}]}`,
		"unknown field":   `{"data":[{"model":"m","type":"tokens","input":1,"ouput":2,"output":2}]}`,
		"duplicate model": `[{"model":"m","type":"tokens","input":1,"output":2},{"model":"m","type":"tokens","input":2,"output":3}]`,
		"empty catalog":   `[]`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeRemotePriceCatalog([]byte(payload)); err == nil {
				t.Fatalf("invalid remote catalog accepted: %s", payload)
			}
		})
	}
}

func TestDecodeRemotePriceCatalogRejectsExplicitNullFields(t *testing.T) {
	for _, body := range []string{
		`[{"model":"a","type":"tokens","input":1,"output":2,"rate_rules":null}]`,
		`[{"model":"a","type":"tokens","input":1,"output":2,"extra_ratios":null}]`,
		`[{"model":"a","type":"tokens","input":1,"output":2,"locked":null}]`,
	} {
		if _, err := decodeRemotePriceCatalog([]byte(body)); err == nil {
			t.Fatalf("expected explicit null field to fail: %s", body)
		}
	}
}
