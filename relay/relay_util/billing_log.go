package relay_util

import "one-api/model"

// 仅记录本次 Token 结算实际使用的规则，不保存请求级配置版本或完整价格目录。
type billingRateRule = model.PriceRuleMatch

type tokenBillingDetails struct {
	UnitsIncludeRules bool                 `json:"units_include_rules"`
	Status            PriceComponentStatus `json:"status"`
	BaseInputRatio    float64              `json:"base_input_ratio"`
	BaseOutputRatio   float64              `json:"base_output_ratio"`
	InputUnits        float64              `json:"input_units"`
	OutputUnits       float64              `json:"output_units"`
	Charge            int64                `json:"charge"`
	Facts             model.PriceRuleFacts `json:"facts"`
	ExtraMultipliers  map[string]float64   `json:"extra_multipliers,omitempty"`
	Rules             []billingRateRule    `json:"rules"`
}
