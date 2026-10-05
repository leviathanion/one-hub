package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"gorm.io/datatypes"
	"io"
	"math"
	"net/http"
	"one-api/common/config"
	"sort"
	"time"
)

const ModelsDevURL = "https://models.dev/api.json"

// Metadata stays outside Price, whose catalog schema is deliberately strict.
type ModelsDevCandidate struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Price    *Price `json:"price,omitempty"`
	Reason   string `json:"reason,omitempty"`
	Conflict bool   `json:"conflict"`
	Selected bool   `json:"selected"`
}
type ModelsDevCatalog struct {
	URL        string               `json:"url"`
	Candidates []ModelsDevCandidate `json:"candidates"`
	Prices     []*Price             `json:"prices"`
	Skipped    int                  `json:"skipped"`
}
type modelsDevCost struct {
	Input       *float64        `json:"input"`
	Output      *float64        `json:"output"`
	CacheRead   *float64        `json:"cache_read"`
	CacheWrite  *float64        `json:"cache_write"`
	Reasoning   *float64        `json:"reasoning"`
	InputAudio  *float64        `json:"input_audio"`
	OutputAudio *float64        `json:"output_audio"`
	Tiers       []modelsDevTier `json:"tiers"`
	Legacy      json.RawMessage `json:"context_over_200k"`
}
type modelsDevTier struct {
	modelsDevCost
	Tier struct {
		Type string `json:"type"`
		Size int    `json:"size"`
	} `json:"tier"`
}

func FetchModelsDevPrices(ctx context.Context) (*ModelsDevCatalog, error) {
	client := &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("models.dev redirects are not allowed") }}
	return fetchModelsDevPrices(ctx, client)
}
func fetchModelsDevPrices(ctx context.Context, client *http.Client) (*ModelsDevCatalog, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ModelsDevURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("models.dev fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models.dev returned status %d", resp.StatusCode)
	}
	return ConvertModelsDevPrices(resp.Body)
}
func ConvertModelsDevPrices(r io.Reader) (*ModelsDevCatalog, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxRemotePriceCatalogBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > MaxRemotePriceCatalogBytes {
		return nil, errors.New("models.dev catalog exceeds size limit")
	}
	var upstream map[string]struct {
		Models map[string]struct {
			Cost json.RawMessage `json:"cost"`
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &upstream); err != nil {
		return nil, fmt.Errorf("models.dev JSON: %w", err)
	}
	if len(upstream) == 0 {
		return nil, errors.New("empty models.dev catalog")
	}
	catalog := &ModelsDevCatalog{URL: ModelsDevURL, Candidates: []ModelsDevCandidate{}}
	counts := map[string]int{}
	for provider, p := range upstream {
		for name, m := range p.Models {
			price, err := convertModelsDevPrice(name, provider, m.Cost)
			candidate := ModelsDevCandidate{Provider: provider, Model: name, Price: price}
			if err != nil {
				candidate.Reason = err.Error()
				candidate.Price = nil
			}
			catalog.Candidates = append(catalog.Candidates, candidate)
			counts[name]++
		}
	}
	for i := range catalog.Candidates {
		catalog.Candidates[i].Conflict = counts[catalog.Candidates[i].Model] > 1
	}
	sort.Slice(catalog.Candidates, func(i, j int) bool {
		a, b := catalog.Candidates[i], catalog.Candidates[j]
		if a.Model != b.Model {
			return a.Model < b.Model
		}
		return a.Provider < b.Provider
	})
	selectModelsDevPrices(catalog)
	return catalog, nil
}

// Official publisher catalogs take precedence over multi-vendor hosts. Keep this
// explicit: SDK names, channel types and low prices do not establish ownership.
// Vertex, Azure and aggregators can host other publishers and are not listed.
var modelsDevOfficialProviders = map[string]bool{
	"anthropic": true, "openai": true, "google": true,
	"xai": true, "deepseek": true, "mistral": true, "cohere": true,
	"meta": true, "llama": true, "moonshotai": true, "zhipuai": true, "zai": true,
	"minimax": true, "alibaba": true, "stepfun": true, "perplexity": true,
}

// Candidates arrive sorted by exact model ID and provider. Never rename IDs or
// choose by price. Ambiguous or invalid preferred sources stay out of the catalog.
func selectModelsDevPrices(catalog *ModelsDevCatalog) {
	catalog.Prices = []*Price{}
	catalog.Skipped = 0
	for start := 0; start < len(catalog.Candidates); {
		end := start + 1
		for end < len(catalog.Candidates) && catalog.Candidates[end].Model == catalog.Candidates[start].Model {
			end++
		}
		preferred, officialCount := start, 0
		for i := start; i < end; i++ {
			if modelsDevOfficialProviders[catalog.Candidates[i].Provider] {
				preferred = i
				officialCount++
			}
		}
		candidate := &catalog.Candidates[preferred]
		if (officialCount == 1 || end-start == 1) && candidate.Price != nil && candidate.Reason == "" {
			candidate.Selected = true
			catalog.Prices = append(catalog.Prices, candidate.Price)
		} else {
			catalog.Skipped++
		}
		start = end
	}
}

func costRatio(value *float64, base float64) (float64, error) {
	if value == nil || math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0 {
		return 0, errors.New("missing or invalid price")
	}
	if base == 0 {
		if *value == 0 {
			return 1, nil
		}
		return 0, errors.New("nonzero price cannot be expressed relative to zero base")
	}
	ratio := *value / base
	if math.IsInf(ratio, 0) {
		return 0, errors.New("price ratio overflows")
	}
	return ratio, nil
}
func decodeModelsDevCost(raw json.RawMessage) (modelsDevCost, error) {
	var c modelsDevCost
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return c, err
	}
	if fields == nil {
		return c, errors.New("missing cost")
	}
	for key := range fields {
		switch key {
		case "input", "output", "cache_read", "cache_write", "reasoning", "input_audio", "output_audio", "tiers", "context_over_200k":
		default:
			return c, fmt.Errorf("unsupported cost field %s", key)
		}
	}
	if tiersRaw, ok := fields["tiers"]; ok {
		var tiers []map[string]json.RawMessage
		if err := json.Unmarshal(tiersRaw, &tiers); err != nil {
			return c, err
		}
		for _, tier := range tiers {
			for key := range tier {
				switch key {
				case "input", "output", "cache_read", "cache_write", "reasoning", "input_audio", "output_audio", "tier":
				default:
					return c, fmt.Errorf("unsupported tier field %s", key)
				}
			}
		}
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, err
	}
	if c.Input == nil || c.Output == nil {
		return c, errors.New("input and output prices are required")
	}
	if _, err := costRatio(c.Input, 1); err != nil {
		return c, err
	}
	if _, err := costRatio(c.Output, 1); err != nil {
		return c, err
	}
	if len(c.Legacy) > 0 && string(c.Legacy) != "null" && len(c.Tiers) == 0 {
		return c, errors.New("legacy context price without tiers is unsupported")
	}
	return c, nil
}
func modelsDevExtras(c modelsDevCost, input, output float64) (map[string]float64, error) {
	extras := map[string]float64{}
	values := []struct {
		value *float64
		base  float64
		keys  []string
	}{
		{c.CacheRead, input, []string{config.UsageExtraCache, config.UsageExtraCacheReadInputTokens}},
		{c.CacheWrite, input, []string{config.UsageExtraCacheWrite, config.UsageExtraCacheCreationInputTokens, config.UsageExtraEphemeral5mInputTokens}},
		{c.Reasoning, output, []string{config.UsageExtraReasoning}},
		{c.InputAudio, input, []string{config.UsageExtraInputAudio}},
		{c.OutputAudio, output, []string{config.UsageExtraOutputAudio}},
	}
	for _, v := range values {
		if v.value == nil {
			continue
		}
		ratio, err := costRatio(v.value, v.base)
		if err != nil {
			return nil, err
		}
		for _, key := range v.keys {
			extras[key] = ratio
		}
	}
	return extras, nil
}
func convertModelsDevPrice(name, provider string, raw json.RawMessage) (*Price, error) {
	c, err := decodeModelsDevCost(raw)
	if err != nil {
		return nil, err
	}
	// Each base rate is an independent absolute multiplier: $0.002/1K = $2/1M.
	p := &Price{Model: name, Type: TokensPriceType, Input: *c.Input / (DollarRate * 1000), Output: *c.Output / (DollarRate * 1000)}
	channelTypes := map[string]int{"openai": config.ChannelTypeOpenAI, "anthropic": config.ChannelTypeAnthropic, "google": config.ChannelTypeGemini, "deepseek": config.ChannelTypeDeepseek, "xai": config.ChannelTypeXAI, "mistral": config.ChannelTypeMistral, "cohere": config.ChannelTypeCohere, "moonshotai": config.ChannelTypeMoonshot, "zhipuai": config.ChannelTypeZhipu, "minimax": config.ChannelTypeMiniMax, "alibaba": config.ChannelTypeAli}
	p.ChannelType = channelTypes[provider]
	extra, err := modelsDevExtras(c, *c.Input, *c.Output)
	if err != nil {
		return nil, err
	}
	if len(extra) > 0 {
		v := datatypes.NewJSONType(extra)
		p.ExtraRatios = &v
	}
	tiers := append([]modelsDevTier(nil), c.Tiers...)
	sort.Slice(tiers, func(i, j int) bool { return tiers[i].Tier.Size < tiers[j].Tier.Size })
	rules := PriceRateRules{Version: 2}
	for i, tier := range tiers {
		if tier.Tier.Type != "context" || tier.Tier.Size <= 0 || (i > 0 && tiers[i-1].Tier.Size == tier.Tier.Size) || len(tier.Tiers) > 0 {
			return nil, errors.New("unsupported context tier")
		}
		in, out := tier.Input, tier.Output
		if in == nil {
			in = c.Input
		}
		if out == nil {
			out = c.Output
		}
		ir, err := costRatio(in, *c.Input)
		if err != nil {
			return nil, err
		}
		or, err := costRatio(out, *c.Output)
		if err != nil {
			return nil, err
		}
		// Extra rule multipliers multiply the base extra ratio, not the tier's input rate.
		te, err := modelsDevExtras(tier.modelsDevCost, *c.Input, *c.Output)
		if err != nil {
			return nil, err
		}
		for key, value := range te {
			base, ok := extra[key]
			if !ok {
				return nil, errors.New("tier extra price lacks a base price")
			}
			r, err := costRatio(&value, base)
			if err != nil {
				return nil, err
			}
			te[key] = r
		}
		for key := range extra {
			if _, ok := te[key]; !ok {
				te[key] = 1
			}
		}
		if value, ok := te[config.UsageExtraCacheCreationInputTokens]; ok {
			te[config.UsageExtraEphemeral1hInputTokens] = value
		}
		if len(te) == 0 {
			te = nil // Match the persisted omitempty representation on repeated imports.
		}
		when := PriceRuleCondition{InputTokens: &PriceTokenRange{GT: &tiers[i].Tier.Size}}
		if i+1 < len(tiers) {
			when.InputTokens.LTE = &tiers[i+1].Tier.Size
		}
		rules.LongContext = append(rules.LongContext, PriceRateRule{ID: fmt.Sprintf("modelsdev-context-%d", tier.Tier.Size), When: when, Multipliers: PriceRateMultiplier{Input: &ir, Output: &or, Extra: te}})
	}
	if len(rules.LongContext) > 0 {
		v := datatypes.NewJSONType(rules)
		p.RateRules = &v
	}
	if err := ValidatePrice(p); err != nil {
		return nil, err
	}
	return p, nil
}
