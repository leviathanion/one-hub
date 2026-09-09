package common

import (
	"fmt"
	"math"
	"one-api/common/config"
)

func LogQuota(quota int) string {
	options := config.GlobalOption.RuntimeSnapshot()
	quotaPerUnit := options.Float64("QuotaPerUnit", config.QuotaPerUnit)
	if options.Bool("DisplayInCurrencyEnabled", config.DisplayInCurrencyEnabled) {
		if quota < 0 {
			return fmt.Sprintf("-＄%.6f 额度", math.Abs(float64(quota)/quotaPerUnit))
		}
		return fmt.Sprintf("＄%.6f 额度", float64(quota)/quotaPerUnit)
	} else {
		return fmt.Sprintf("%d 点额度", quota)
	}
}
