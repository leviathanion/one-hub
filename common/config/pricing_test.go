package config

import (
	"fmt"
	"strings"
	"testing"

	"github.com/spf13/viper"
)

func TestPricingRequiredModelsAreExactAndUnique(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	viper.Set("pricing.required_models", []string{"gpt-5.6", "gpt-5.6-sol"})
	models, err := PricingRequiredModels()
	if err != nil || len(models) != 2 {
		t.Fatalf("valid required models rejected: models=%v err=%v", models, err)
	}
	for _, invalid := range [][]string{{"gpt-*"}, {"gpt-5.6", "gpt-5.6"}, {" "}} {
		viper.Set("pricing.required_models", invalid)
		if _, err := PricingRequiredModels(); err == nil {
			t.Fatalf("invalid required models accepted: %v", invalid)
		}
	}
	overlong := strings.Repeat("x", MaxPricingModelRunes+1)
	viper.Set("pricing.required_models", []string{overlong})
	if _, err := PricingRequiredModels(); err == nil {
		t.Fatal("overlong required model was accepted")
	}
	tooMany := make([]string, maxPricingRequiredModels+1)
	for index := range tooMany {
		tooMany[index] = fmt.Sprintf("model-%d", index)
	}
	viper.Set("pricing.required_models", tooMany)
	if _, err := PricingRequiredModels(); err == nil {
		t.Fatal("unbounded required model list was accepted")
	}
}
