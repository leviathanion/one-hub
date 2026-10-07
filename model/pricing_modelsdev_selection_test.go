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
		{"canonical publisher beats mixed catalog", `{"alibaba":{"models":{"kimi-k3":{"canonical_model_id":"moonshotai/kimi-k3","cost":{"input":1,"output":2}}}},"moonshotai":{"models":{"kimi-k3":{"canonical_model_id":"moonshotai/kimi-k3","cost":{"input":3,"output":15}}}}}`, "moonshotai", 0},
		{"mixed catalog does not establish ownership", `{"alibaba":{"models":{"deepseek-v4-flash-0731":{"canonical_model_id":"deepseek/deepseek-v4-flash-0731","cost":{"input":1,"output":2}}}},"host":{"models":{"deepseek-v4-flash-0731":{"cost":{"input":0,"output":0}}}}}`, "", 1},
		{"official mixed provider retains own models", `{"alibaba":{"models":{"qwen-model":{"canonical_model_id":"alibaba/qwen-model","cost":{"input":1,"output":2}}}},"host":{"models":{"qwen-model":{"cost":{"input":0,"output":0}}}}}`, "alibaba", 0},
		{"publisher provider alias", `{"zai":{"models":{"glm-model":{"canonical_model_id":"zhipuai/glm-model","cost":{"input":1,"output":2}}}},"host":{"models":{"glm-model":{"cost":{"input":0,"output":0}}}}}`, "zai", 0},
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
					if candidate.Reason != "" {
						t.Fatalf("selected source has rejection reason: %+v", candidate)
					}
					if selected != "" {
						t.Fatal("selected more than one provider")
					}
					selected = candidate.Provider
					if len(catalog.Prices) != 1 || catalog.Prices[0] != candidate.Price {
						t.Fatal("catalog differs from selected candidate")
					}
				} else {
					if candidate.Reason == "" {
						t.Fatalf("unselected source lacks reason: %+v", candidate)
					}
					if candidate.Price == nil {
						if candidate.Reason != "input and output prices are required" {
							t.Fatalf("conversion error was lost: %+v", candidate)
						}
					} else {
						want := "ambiguous providers"
						if tc.provider != "" {
							want = "another provider selected"
						}
						if tc.name == "invalid official never falls back to host" {
							want = "official provider price is invalid"
						}
						if candidate.Reason != want {
							t.Fatalf("reason=%q want=%q", candidate.Reason, want)
						}
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
