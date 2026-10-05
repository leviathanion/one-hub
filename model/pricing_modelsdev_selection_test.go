package model

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestModelsDevSyncCatalogSourceSelection(t *testing.T) {
	tests := []struct {
		name, upstream, provider string
		skipped                  int
	}{
		{"official beats discounted host", `{"openai":{"models":{"same":{"cost":{"input":4,"output":8}}}},"azure":{"models":{"same":{"cost":{"input":0,"output":0}}}}}`, "openai", 0},
		{"official free price stays free", `{"openai":{"models":{"same":{"cost":{"input":0,"output":0}}}},"host":{"models":{"same":{"cost":{"input":4,"output":8}}}}}`, "openai", 0},
		{"unknown single source", `{"host":{"models":{"same":{"cost":{"input":4,"output":8}}}}}`, "host", 0},
		{"ambiguous hosts are skipped", `{"a":{"models":{"same":{"cost":{"input":4,"output":8}}}},"b":{"models":{"same":{"cost":{"input":2,"output":8}}}}}`, "", 1},
		{"multiple publishers are skipped", `{"openai":{"models":{"same":{"cost":{"input":4,"output":8}}}},"anthropic":{"models":{"same":{"cost":{"input":2,"output":8}}}}}`, "", 1},
		{"invalid official never falls back to host", `{"openai":{"models":{"same":{"cost":{"input":4}}}},"host":{"models":{"same":{"cost":{"input":2,"output":8}}}}}`, "", 1},
		{"invalid single source", `{"host":{"models":{"same":{"cost":{"input":4}}}}}`, "", 1},
		{"vertex hosted model is not first party", `{"google-vertex":{"models":{"same":{"cost":{"input":4,"output":8}}}},"anthropic":{"models":{"same":{"cost":{"input":2,"output":8}}}}}`, "anthropic", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			catalog, err := ConvertModelsDevPrices(strings.NewReader(tc.upstream))
			if err != nil {
				t.Fatal(err)
			}
			if catalog.Skipped != tc.skipped {
				t.Fatalf("skipped=%d", catalog.Skipped)
			}
			selected := ""
			for _, candidate := range catalog.Candidates {
				if candidate.Selected {
					if selected != "" {
						t.Fatal("selected more than one provider")
					}
					selected = candidate.Provider
					if len(catalog.Prices) != 1 || catalog.Prices[0] != candidate.Price {
						t.Fatal("catalog differs from selected candidate")
					}
				}
			}
			if selected != tc.provider {
				t.Fatalf("selected=%q want=%q", selected, tc.provider)
			}
			if tc.name == "official free price stays free" && (catalog.Prices[0].Input != 0 || catalog.Prices[0].Output != 0) {
				t.Fatal("official free price changed")
			}
			if tc.provider == "" && len(catalog.Prices) != 0 {
				t.Fatal("ambiguous catalog is not empty")
			}
			// Empty sources must encode as [], and source prices must retain the
			// strict sync schema without candidate provenance fields.
			raw, err := json.Marshal(catalog.Prices)
			if err != nil {
				t.Fatal(err)
			}
			if string(raw) == "null" || strings.Contains(string(raw), `"provider"`) || strings.Contains(string(raw), `"selected"`) || strings.Contains(string(raw), `"reason"`) || strings.Contains(string(raw), `"conflict"`) {
				t.Fatal(string(raw))
			}
		})
	}
}

func TestModelsDevSyncCatalogStableExactModelIDs(t *testing.T) {
	raw := `{"host":{"models":{"vendor/a":{"cost":{"input":2,"output":4}},"a":{"cost":{"input":2,"output":4}}}},"openai":{"models":{"z":{"cost":{"input":0,"output":0}}}}}`
	for i := 0; i < 20; i++ {
		catalog, err := ConvertModelsDevPrices(strings.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		if len(catalog.Prices) != 3 || catalog.Prices[0].Model != "a" || catalog.Prices[1].Model != "vendor/a" || catalog.Prices[2].Model != "z" {
			t.Fatalf("unexpected catalog: %+v", catalog)
		}
	}
}
