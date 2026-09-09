package controller

import (
	"one-api/common/config"
	"one-api/model"
	"testing"
)

func TestCalculateOrderAmountReadsCurrentRuntimeOptions(t *testing.T) {
	originalManager := config.GlobalOption
	t.Cleanup(func() { config.GlobalOption = originalManager })

	manager := config.NewOptionManager()
	usdRate := 7.3
	rechargeDiscount := ""
	manager.RegisterFloat("PaymentUSDRate", &usdRate)
	manager.RegisterString("RechargeDiscount", &rechargeDiscount)
	config.GlobalOption = manager

	if _, err := manager.PublishRuntimeOverrides(1, map[string]string{
		"PaymentUSDRate":   "7",
		"RechargeDiscount": `{"100":0.8}`,
	}); err != nil {
		t.Fatalf("publish first options: %v", err)
	}
	payment := &model.Payment{Currency: model.CurrencyTypeCNY}
	discount, fee, payMoney := calculateOrderAmount(payment, 100)
	if discount != 140 || fee != 0 || payMoney != 560 {
		t.Fatalf("unexpected first calculation: discount=%v fee=%v pay=%v", discount, fee, payMoney)
	}

	if _, err := manager.PublishRuntimeOverrides(2, map[string]string{
		"PaymentUSDRate":   "8",
		"RechargeDiscount": `{"100":0.5}`,
	}); err != nil {
		t.Fatalf("publish second options: %v", err)
	}
	discount, fee, payMoney = calculateOrderAmount(payment, 100)
	if discount != 400 || fee != 0 || payMoney != 400 {
		t.Fatalf("calculation did not observe current options: discount=%v fee=%v pay=%v", discount, fee, payMoney)
	}
}
