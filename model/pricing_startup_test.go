package model

import (
	"context"
	"testing"

	"github.com/spf13/viper"
)

func TestPricingStartupInitializesOnlyEmptyDatabase(t *testing.T) {
	for _, existing := range []bool{false, true} {
		name := "empty database"
		if existing {
			name = "existing prices"
		}
		t.Run(name, func(t *testing.T) {
			db, _ := setupVersionedPricingTest(t)
			oldPricing := PricingInstance
			t.Cleanup(func() { PricingInstance = oldPricing })
			// 残留的旧配置不能恢复自动更新，也不能阻止空库初始化。
			oldMode, oldEnabled := viper.Get("auto_price_updates_mode"), viper.Get("auto_price_updates")
			mode := "overwrite"
			if existing {
				mode = "system"
			}
			viper.Set("auto_price_updates_mode", mode)
			viper.Set("auto_price_updates", true)
			t.Cleanup(func() {
				viper.Set("auto_price_updates_mode", oldMode)
				viper.Set("auto_price_updates", oldEnabled)
			})
			if existing {
				if err := db.Create(&Price{Model: "local-only", Type: TokensPriceType, Input: 7, Output: 8, Locked: true}).Error; err != nil {
					t.Fatal(err)
				}
			}
			before, err := ReadPublicationVersion(context.Background(), db, PublicationOwnerPrice)
			if err != nil {
				t.Fatal(err)
			}

			NewPricing()

			if PricingInstance.IsDegraded() {
				t.Fatal("startup price publication is degraded")
			}
			prices := PricingInstance.GetAllPrices()
			if existing {
				got := prices["local-only"]
				if len(prices) != 1 || got == nil || got.Input != 7 || got.Output != 8 || !got.Locked || PricingInstance.PublishedVersion() != before {
					t.Fatalf("startup changed existing pricing: prices=%+v version=%d", prices, PricingInstance.PublishedVersion())
				}
			} else if len(prices) != len(GetDefaultPrice()) || PricingInstance.PublishedVersion() != before+1 {
				t.Fatalf("empty database did not publish defaults: count=%d version=%d", len(prices), PricingInstance.PublishedVersion())
			}
		})
	}
}
