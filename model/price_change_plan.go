package model

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"

	"gorm.io/datatypes"
	"gorm.io/gorm"
)

type PriceChangeAction string

const (
	PriceChangeAdd    PriceChangeAction = "add"
	PriceChangeUpdate PriceChangeAction = "update"
	PriceChangeDelete PriceChangeAction = "delete"
	PriceChangeLocked PriceChangeAction = "locked"
)

var ErrPriceChangePlanMismatch = errors.New("price change plan no longer matches preview")

// PricePolicyView is the complete remotely syncable Price policy. Pointer
// fields preserve the distinction between an absent policy and an explicit
// empty object in the canonical preview and digest.
type PricePolicyView struct {
	Model       string              `json:"model"`
	Type        string              `json:"type"`
	ChannelType int                 `json:"channel_type"`
	Input       float64             `json:"input"`
	Output      float64             `json:"output"`
	Locked      bool                `json:"locked"`
	ExtraRatios *map[string]float64 `json:"extra_ratios"`
	RateRules   *PriceRateRules     `json:"rate_rules"`
}

type PriceChange struct {
	Action PriceChangeAction `json:"action"`
	Model  string            `json:"model"`
	Before *PricePolicyView  `json:"before"`
	After  *PricePolicyView  `json:"after"`
}

type PriceChangePlan struct {
	Mode    PriceUpdateMode `json:"mode"`
	Changes []PriceChange   `json:"changes"`
}

type PriceChangePreview struct {
	BaseVersion int64           `json:"base_version"`
	Plan        PriceChangePlan `json:"plan"`
	Digest      string          `json:"digest"`
}

func PreviewPriceChange(ctx context.Context, source []*Price, mode PriceUpdateMode) (*PriceChangePreview, error) {
	if DB == nil {
		return nil, errors.New("database is required")
	}
	if err := validatePriceChangeInput(source, mode); err != nil {
		return nil, err
	}
	var preview *PriceChangePreview
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		baseVersion, err := ReadPublicationVersion(ctx, tx, PublicationOwnerPrice)
		if err != nil {
			return err
		}
		current, err := loadPriceMap(tx)
		if err != nil {
			return err
		}
		confirmedVersion, err := ReadPublicationVersion(ctx, tx, PublicationOwnerPrice)
		if err != nil {
			return err
		}
		if confirmedVersion != baseVersion {
			return ErrPublicationVersionConflict
		}
		plan := buildPriceChangePlan(current, source, mode)
		digest, err := digestPriceChange(baseVersion, source, plan)
		if err != nil {
			return err
		}
		preview = &PriceChangePreview{BaseVersion: baseVersion, Plan: plan, Digest: digest}
		return nil
	})
	return preview, err
}

func ApplyPriceChange(ctx context.Context, publisher *Pricing, source []*Price, mode PriceUpdateMode, baseVersion int64, digest string) (int64, error) {
	if publisher == nil || DB == nil {
		return 0, errors.New("pricing publisher is required")
	}
	if baseVersion < 1 || digest == "" {
		return 0, errors.New("base_version and digest are required")
	}
	if err := validatePriceChangeInput(source, mode); err != nil {
		return 0, err
	}
	var appliedPlan PriceChangePlan
	planReady := false
	changed := false
	tx := DB.WithContext(ctx).Begin()
	if tx.Error != nil {
		return 0, tx.Error
	}
	rollback := true
	defer func() {
		if rollback {
			_ = tx.Rollback().Error
		}
	}()
	head, err := ReadPublicationVersion(ctx, tx, PublicationOwnerPrice)
	if err != nil {
		return 0, err
	}
	if head != baseVersion {
		return 0, ErrPublicationVersionConflict
	}
	current, err := loadPriceMap(tx)
	if err != nil {
		return 0, err
	}
	appliedPlan = buildPriceChangePlan(current, source, mode)
	actualDigest, err := digestPriceChange(baseVersion, source, appliedPlan)
	if err != nil {
		return 0, err
	}
	if !equalPriceChangeDigest(digest, actualDigest) {
		return 0, ErrPriceChangePlanMismatch
	}
	planReady = true
	for _, change := range appliedPlan.Changes {
		switch change.Action {
		case PriceChangeAdd:
			if err := tx.Create(change.After.price()).Error; err != nil {
				return 0, err
			}
			changed = true
		case PriceChangeUpdate:
			result := tx.Model(&Price{}).Where("model = ? AND locked = ?", change.Model, false).
				Select("type", "channel_type", "input", "output", "locked", "extra_ratios", "rate_rules").
				Updates(change.After.price())
			if result.Error != nil {
				return 0, result.Error
			}
			if result.RowsAffected != 1 {
				return 0, ErrPublicationVersionConflict
			}
			changed = true
		case PriceChangeDelete:
			result := tx.Where("model = ? AND locked = ?", change.Model, false).Delete(&Price{})
			if result.Error != nil {
				return 0, result.Error
			}
			if result.RowsAffected != 1 {
				return 0, ErrPublicationVersionConflict
			}
			changed = true
		case PriceChangeLocked:
			// Locked rows are part of the canonical plan but are never mutated.
		default:
			return 0, fmt.Errorf("unsupported price change action %q", change.Action)
		}
	}
	if !changed {
		_ = tx.Rollback().Error
		rollback = false
		return baseVersion, nil
	}
	targetState, err := loadPricePolicyState(tx)
	if err != nil {
		return 0, err
	}
	if _, err := BumpPublicationVersionCAS(ctx, tx, PublicationOwnerPrice, baseVersion); err != nil {
		return 0, err
	}
	rollback = false
	commitErr := tx.Commit().Error
	if commitErr != nil {
		if !planReady || errors.Is(commitErr, ErrPublicationVersionConflict) || errors.Is(commitErr, ErrPriceChangePlanMismatch) {
			return 0, commitErr
		}
		if resolvedErr := resolvePriceCommitOutcome(ctx, baseVersion, commitErr, func(probe *gorm.DB) (bool, error) {
			actual, err := loadPricePolicyState(probe)
			return reflect.DeepEqual(actual, targetState), err
		}); resolvedErr != nil {
			return 0, resolvedErr
		}
	}
	newVersion := baseVersion + 1
	publisher.convergeAfterPriceCommit(ctx, newVersion)
	return newVersion, nil
}

func validatePriceChangeInput(source []*Price, mode PriceUpdateMode) error {
	switch mode {
	case PriceUpdateModeAdd, PriceUpdateModeUpdate, PriceUpdateModeOverwrite:
	default:
		return fmt.Errorf("unsupported price update mode %q", mode)
	}
	return validatePriceInputSet(source)
}

func buildPriceChangePlan(current map[string]*Price, source []*Price, mode PriceUpdateMode) PriceChangePlan {
	plan := PriceChangePlan{Mode: mode, Changes: make([]PriceChange, 0)}
	sourceByModel := make(map[string]*Price, len(source))
	for _, incoming := range source {
		sourceByModel[incoming.Model] = incoming
		existing, exists := current[incoming.Model]
		if !exists {
			if mode == PriceUpdateModeAdd || mode == PriceUpdateModeOverwrite {
				after := pricePolicyView(priceForSync(incoming, nil))
				plan.Changes = append(plan.Changes, PriceChange{Action: PriceChangeAdd, Model: incoming.Model, After: &after})
			}
			continue
		}
		if mode == PriceUpdateModeAdd {
			continue
		}
		after := pricePolicyView(priceForSync(incoming, existing))
		before := pricePolicyView(existing)
		if reflect.DeepEqual(before, after) {
			continue
		}
		action := PriceChangeUpdate
		if existing.Locked {
			action = PriceChangeLocked
		}
		plan.Changes = append(plan.Changes, PriceChange{Action: action, Model: incoming.Model, Before: &before, After: &after})
	}
	if mode == PriceUpdateModeOverwrite {
		for modelName, existing := range current {
			if _, exists := sourceByModel[modelName]; exists {
				continue
			}
			before := pricePolicyView(existing)
			action := PriceChangeDelete
			if existing.Locked {
				action = PriceChangeLocked
			}
			plan.Changes = append(plan.Changes, PriceChange{Action: action, Model: modelName, Before: &before})
		}
	}
	sort.Slice(plan.Changes, func(i, j int) bool {
		if plan.Changes[i].Model != plan.Changes[j].Model {
			return plan.Changes[i].Model < plan.Changes[j].Model
		}
		return plan.Changes[i].Action < plan.Changes[j].Action
	})
	return plan
}

func digestPriceChange(baseVersion int64, source []*Price, plan PriceChangePlan) (string, error) {
	canonicalSource := make([]PricePolicyView, 0, len(source))
	for _, price := range source {
		canonicalSource = append(canonicalSource, pricePolicyView(price))
	}
	sort.Slice(canonicalSource, func(i, j int) bool { return canonicalSource[i].Model < canonicalSource[j].Model })
	payload := struct {
		BaseVersion int64             `json:"base_version"`
		Source      []PricePolicyView `json:"source"`
		Plan        PriceChangePlan   `json:"plan"`
	}{BaseVersion: baseVersion, Source: canonicalSource, Plan: plan}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func equalPriceChangeDigest(expected, actual string) bool {
	expectedBytes, expectedErr := hex.DecodeString(expected)
	actualBytes, actualErr := hex.DecodeString(actual)
	if expectedErr != nil || actualErr != nil || len(expectedBytes) != sha256.Size || len(actualBytes) != sha256.Size {
		return false
	}
	return subtle.ConstantTimeCompare(expectedBytes, actualBytes) == 1
}

func pricePolicyView(price *Price) PricePolicyView {
	if price == nil {
		return PricePolicyView{}
	}
	view := PricePolicyView{
		Model:       price.Model,
		Type:        price.Type,
		ChannelType: price.ChannelType,
		Input:       price.Input,
		Output:      price.Output,
		Locked:      price.Locked,
	}
	if price.ExtraRatios != nil {
		copied := make(map[string]float64, len(price.ExtraRatios.Data()))
		for name, ratio := range price.ExtraRatios.Data() {
			copied[name] = ratio
		}
		view.ExtraRatios = &copied
	}
	if price.RateRules != nil {
		rules := ClonePriceRateRules(price.RateRules.Data())
		view.RateRules = &rules
	}
	return view
}

func (view *PricePolicyView) price() *Price {
	if view == nil {
		return nil
	}
	price := &Price{
		Model:       view.Model,
		Type:        view.Type,
		ChannelType: view.ChannelType,
		Input:       view.Input,
		Output:      view.Output,
		Locked:      view.Locked,
	}
	if view.ExtraRatios != nil {
		extra := make(map[string]float64, len(*view.ExtraRatios))
		for name, ratio := range *view.ExtraRatios {
			extra[name] = ratio
		}
		encoded := datatypes.NewJSONType(extra)
		price.ExtraRatios = &encoded
	}
	if view.RateRules != nil {
		encoded := datatypes.NewJSONType(ClonePriceRateRules(*view.RateRules))
		price.RateRules = &encoded
	}
	return price
}
