package config

import (
	"errors"
	"strings"

	"github.com/spf13/viper"
)

const (
	maxPricingRequiredModels = 256
	MaxPricingModelRunes     = 100
)

func PricingRequiredModels() ([]string, error) {
	raw := viper.GetStringSlice("pricing.required_models")
	if len(raw) > maxPricingRequiredModels {
		return nil, errors.New("pricing.required_models contains too many models")
	}
	models := make([]string, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for _, value := range raw {
		model := strings.TrimSpace(value)
		if model == "" {
			return nil, errors.New("pricing.required_models must not contain empty values")
		}
		if strings.Contains(model, "*") {
			return nil, errors.New("pricing.required_models accepts exact models only")
		}
		if len([]rune(model)) > MaxPricingModelRunes {
			return nil, errors.New("pricing.required_models contains an overlong model")
		}
		if _, exists := seen[model]; exists {
			return nil, errors.New("pricing.required_models must not contain duplicates")
		}
		seen[model] = struct{}{}
		models = append(models, model)
	}
	return models, nil
}
