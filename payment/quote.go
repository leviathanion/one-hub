package payment

import (
	"encoding/json"
	"errors"
	"github.com/shopspring/decimal"
	"one-api/common"
	"one-api/common/config"
	"one-api/model"
	"one-api/payment/types"
	"strconv"
)

type Quote struct {
	Total    types.Money
	Quota    int64
	Fee      float64
	Discount float64
	Details  string
}

// 折扣、手续费和汇率沿既有顺序计算，只在最终现金金额落单时 HALF_UP。
func CalculateQuote(p *model.Payment, amount int) (Quote, error) {
	options := config.GlobalOption.RuntimeSnapshot()
	if amount <= 0 || amount < options.Int("PaymentMinAmount", config.PaymentMinAmount) {
		return Quote{}, errors.New("充值金额小于允许的最小金额")
	}
	exponent, err := types.CurrencyExponent(string(p.Currency))
	if err != nil {
		return Quote{}, err
	}
	discount := decimal.NewFromFloat(common.GetRechargeDiscountFromSnapshot(options, strconv.Itoa(amount)))
	rate := decimal.NewFromInt(1)
	if p.Currency == model.CurrencyTypeCNY {
		rate = decimal.NewFromFloat(options.Float64("PaymentUSDRate", config.PaymentUSDRate))
	}
	if discount.Sign() <= 0 || rate.Sign() <= 0 || p.FixedFee < 0 || p.PercentFee < 0 {
		return Quote{}, errors.New("支付报价配置无效")
	}
	base := decimal.NewFromInt(int64(amount))
	discounted := base.Mul(discount)
	fee := decimal.Zero
	oldTotal := base
	if p.PercentFee > 0 {
		percent := decimal.NewFromFloat(p.PercentFee)
		fee = discounted.Mul(percent)
		oldTotal = base.Mul(decimal.NewFromInt(1).Add(percent))
	} else if p.FixedFee > 0 {
		fee = decimal.NewFromFloat(p.FixedFee)
		oldTotal = base.Add(fee)
	}
	cash := discounted.Add(fee).Mul(rate).Round(int32(exponent))
	minor := cash.Shift(int32(exponent))
	if !minor.IsInteger() || !minor.Equal(decimal.NewFromInt(minor.IntPart())) {
		return Quote{}, errors.New("充值金额超出范围")
	}
	total := types.Money{Minor: minor.IntPart(), Currency: string(p.Currency), Exponent: exponent}
	if err := total.Validate(); err != nil {
		return Quote{}, err
	}
	quotaValue := base.Mul(decimal.NewFromFloat(options.Float64("QuotaPerUnit", config.QuotaPerUnit)))
	if !quotaValue.IsInteger() || quotaValue.Sign() <= 0 || !quotaValue.Equal(decimal.NewFromInt(quotaValue.IntPart())) || int64(int(quotaValue.IntPart())) != quotaValue.IntPart() {
		return Quote{}, errors.New("充值额度无效")
	}
	details, _ := json.Marshal(map[string]string{"discount": discount.String(), "fee": fee.String(), "exchange_rate": rate.String(), "rounding": "final_half_up"})
	return Quote{Total: total, Quota: quotaValue.IntPart(), Fee: fee.InexactFloat64(), Discount: oldTotal.Mul(rate).Sub(cash).InexactFloat64(), Details: string(details)}, nil
}
