package common

import (
	"encoding/json"
	"one-api/common/config"
	"one-api/common/logger"
)

var RechargeDiscount = map[string]float64{}
var SafeKeyword = map[string]string{}

func RechargeDiscount2JSONString() string {
	jsonBytes, err := json.Marshal(RechargeDiscount)
	if err != nil {
		logger.SysError("error marshalling recharge discount: " + err.Error())
	}
	return string(jsonBytes)
}

func SafeKeyword2JSONString() string {
	jsonBytes, err := json.Marshal(SafeKeyword)
	if err != nil {
		logger.SysError("error marshalling recharge discount: " + err.Error())
	}
	return string(jsonBytes)
}
func UpdateSafeKeywordByJSONString(jsonStr string) error {
	SafeKeyword = map[string]string{}
	return json.Unmarshal([]byte(jsonStr), &SafeKeyword)
}
func UpdateRechargeDiscountByJSONString(jsonStr string) error {
	next := make(map[string]float64)
	if err := json.Unmarshal([]byte(jsonStr), &next); err != nil {
		return err
	}
	RechargeDiscount = next
	return nil
}

func GetRechargeDiscount(name string) float64 {
	return GetRechargeDiscountFromSnapshot(config.GlobalOption.RuntimeSnapshot(), name)
}

func GetRechargeDiscountFromSnapshot(options *config.RuntimeOptionsSnapshot, name string) float64 {
	discounts := make(map[string]float64)
	if options == nil {
		options = config.GlobalOption.RuntimeSnapshot()
	}
	raw := options.String("RechargeDiscount", config.RechargeDiscount)
	if err := json.Unmarshal([]byte(raw), &discounts); err != nil {
		logger.SysError("invalid recharge discount runtime option: " + err.Error())
		return 1
	}
	ratio, ok := discounts[name]
	if !ok {
		logger.SysError("recharge discount not found: " + name)
		return 1
	}
	return ratio
}
