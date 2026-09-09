package model

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/datatypes"
)

const pricePublicationWatchInterval = 5 * time.Second

func CheckPricePublication(ctx context.Context, requiredModels []string) error {
	if DB == nil || PricingInstance == nil {
		return errors.New("price publication is not initialized")
	}
	migrator := DB.WithContext(ctx).Migrator()
	if !migrator.HasTable(&Price{}) {
		return errors.New("prices table is missing")
	}
	for _, column := range []string{"model", "type", "channel_type", "input", "output", "locked", "extra_ratios", "rate_rules"} {
		if !migrator.HasColumn(&Price{}, column) {
			return fmt.Errorf("prices column %q is missing", column)
		}
	}
	if !migrator.HasIndex(&Price{}, "idx_prices_model_unique") {
		return errors.New("prices model unique index is missing")
	}
	head, err := ReadPublicationVersion(ctx, DB, PublicationOwnerPrice)
	if err != nil {
		return err
	}
	version := PricingInstance.PublishedVersion()
	if version < 1 {
		return errors.New("prices have not been published")
	}
	if version != head {
		return fmt.Errorf("published price version %d does not match database head %d", version, head)
	}
	for _, modelName := range requiredModels {
		if _, ok := PricingInstance.FindExactPrice(modelName); !ok {
			return fmt.Errorf("required exact price %q is missing", modelName)
		}
	}
	return nil
}

func SyncPricePublication(ctx context.Context) error {
	if PricingInstance == nil {
		return errors.New("prices are not initialized")
	}
	head, err := ReadPublicationVersion(ctx, DB, PublicationOwnerPrice)
	if err != nil {
		PricingInstance.setPublicationError(err)
		return err
	}
	if PricingInstance.PublishedVersion() == head {
		PricingInstance.setPublicationError(nil)
		return nil
	}
	return PricingInstance.InitContext(ctx)
}

func WatchPricePublication(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	ticker := time.NewTicker(pricePublicationWatchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			probeCtx, cancelProbe := context.WithTimeout(ctx, publicationCommitProbeDeadline)
			_ = SyncPricePublication(probeCtx)
			cancelProbe()
		}
	}
}

func (p *Pricing) PublishedVersion() int64 {
	if p == nil {
		return 0
	}
	p.RLock()
	defer p.RUnlock()
	return p.publishedVersion
}

// replacePublication installs a completely loaded map under one lock. Readers
// never observe a partially reloaded table, but no request retains this state.
func (p *Pricing) replacePublication(version int64, prices map[string]*Price, match []string) bool {
	if p == nil {
		return false
	}
	p.Lock()
	defer p.Unlock()
	if p.publishedVersion >= version {
		return false
	}
	p.Prices = prices
	p.Match = append([]string(nil), match...)
	p.publishedVersion = version
	return true
}

func clonePricePolicy(price Price) Price {
	cloned := price
	if price.ExtraRatios != nil {
		raw := price.ExtraRatios.Data()
		copied := make(map[string]float64, len(raw))
		for key, value := range raw {
			copied[key] = value
		}
		extra := datatypes.NewJSONType(copied)
		cloned.ExtraRatios = &extra
	}
	if price.RateRules != nil {
		rules := datatypes.NewJSONType(ClonePriceRateRules(price.RateRules.Data()))
		cloned.RateRules = &rules
	}
	if price.ModelInfo != nil {
		info := *price.ModelInfo
		info.InputModalities = append([]string(nil), price.ModelInfo.InputModalities...)
		info.OutputModalities = append([]string(nil), price.ModelInfo.OutputModalities...)
		info.Tags = append([]string(nil), price.ModelInfo.Tags...)
		info.SupportUrl = append([]string(nil), price.ModelInfo.SupportUrl...)
		cloned.ModelInfo = &info
	}
	return cloned
}
