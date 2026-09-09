package types

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

func CurrencyExponent(currency string) (int, error) {
	if currency == "CNY" || currency == "USD" {
		return 2, nil
	}
	return 0, errors.New("不支持的支付币种")
}

// ParseMoney 只接受协议中的十进制非负金额；超精度不会被截断或容差接受。
func ParseMoney(value, currency string) (Money, error) {
	exponent, err := CurrencyExponent(currency)
	if err != nil {
		return Money{}, err
	}
	if value == "" || strings.TrimSpace(value) != value {
		return Money{}, errors.New("缺少或无效的支付金额")
	}
	parts := strings.Split(value, ".")
	if len(parts) > 2 || parts[0] == "" {
		return Money{}, errors.New("无效的支付金额")
	}
	for _, part := range parts {
		if part == "" {
			return Money{}, errors.New("无效的支付金额")
		}
		for _, c := range part {
			if c < '0' || c > '9' {
				return Money{}, errors.New("无效的支付金额")
			}
		}
	}
	fraction := ""
	if len(parts) == 2 {
		fraction = parts[1]
	}
	if len(fraction) > exponent {
		return Money{}, errors.New("支付金额超过币种精度")
	}
	fraction += strings.Repeat("0", exponent-len(fraction))
	minor, err := strconv.ParseInt(parts[0]+fraction, 10, 64)
	if err != nil {
		return Money{}, err
	}
	return Money{Minor: minor, Currency: currency, Exponent: exponent}, nil
}
func (m Money) Validate() error {
	exp, err := CurrencyExponent(m.Currency)
	if err != nil {
		return err
	}
	if m.Exponent != exp || m.Minor <= 0 {
		return errors.New("订单金额必须为支持精度的正数")
	}
	return nil
}
func (m Money) DecimalString() string {
	scale := int64(math.Pow10(m.Exponent))
	return fmt.Sprintf("%d.%0*d", m.Minor/scale, m.Exponent, m.Minor%scale)
}
