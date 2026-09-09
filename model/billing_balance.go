package model

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"one-api/common/config"
	"one-api/common/utils"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type BillingBalanceOutcome string

const (
	BillingBalanceCommitted            BillingBalanceOutcome = "committed"
	BillingBalanceDefinitelyRolledBack BillingBalanceOutcome = "definitely_rolled_back"
	BillingBalanceCommitUnknown        BillingBalanceOutcome = "commit_unknown"
)

type BillingBalanceResult struct {
	Outcome           BillingBalanceOutcome
	TokenQuotaApplied bool
	CommitAttempted   bool
}

var (
	ErrBillingUserUnavailable       = errors.New("billing user is unavailable")
	ErrBillingUserQuotaInsufficient = errors.New("billing user quota is insufficient")
	ErrBillingTokenUnavailable      = errors.New("billing token is unavailable")
	ErrBillingOwnership             = errors.New("billing token ownership mismatch")
	ErrBillingRange                 = errors.New("billing balance range invariant failed")
)

// CheckBillingAdmission validates the current SQL billing principal without
// reserving quota. SQL remains the admission authority even for zero-price
// operations; caches are projections only.
func CheckBillingAdmission(ctx context.Context, userID, tokenID int) (BillingBalanceResult, error) {
	return applyBillingBalanceTransaction(ctx, func(tx *gorm.DB) (bool, error) {
		return checkBillingAdmissionInTransaction(tx, userID, tokenID)
	})
}

func checkBillingAdmissionInTransaction(tx *gorm.DB, userID, tokenID int) (bool, error) {
	user, token, err := lockBillingPrincipal(tx, userID, tokenID, false)
	if err != nil {
		return false, err
	}
	if user.Status != config.UserStatusEnabled {
		return false, ErrBillingUserUnavailable
	}
	if user.Quota <= 0 {
		return false, ErrBillingUserQuotaInsufficient
	}
	if token.Status != config.TokenStatusEnabled || (token.ExpiredTime != -1 && token.ExpiredTime < time.Now().Unix()) {
		return false, ErrBillingTokenUnavailable
	}
	if !token.UnlimitedQuota && token.RemainQuota <= 0 {
		return false, ErrTokenQuotaInsufficient
	}
	return false, nil
}

func ApplyBillingReserve(ctx context.Context, userID, tokenID int, quota int64) (BillingBalanceResult, error) {
	if quota <= 0 {
		return BillingBalanceResult{Outcome: BillingBalanceDefinitelyRolledBack}, errors.New("billing reserve quota must be positive")
	}
	if quota > int64(math.MaxInt) {
		return BillingBalanceResult{Outcome: BillingBalanceDefinitelyRolledBack}, ErrBillingRange
	}
	return applyBillingBalanceTransaction(ctx, func(tx *gorm.DB) (bool, error) {
		return ApplyBillingReserveInTransaction(tx, userID, tokenID, quota)
	})
}

func ApplyBillingReserveInTransaction(tx *gorm.DB, userID, tokenID int, quota int64) (bool, error) {
	if quota < 0 || quota > int64(math.MaxInt) {
		return false, ErrBillingRange
	}
	user, token, err := lockBillingPrincipal(tx, userID, tokenID, false)
	if err != nil {
		return false, err
	}
	amount := int(quota)
	if user.Status != config.UserStatusEnabled {
		return false, ErrBillingUserUnavailable
	}
	if user.Quota < amount {
		return false, ErrBillingUserQuotaInsufficient
	}
	if token.Status != config.TokenStatusEnabled || (token.ExpiredTime != -1 && token.ExpiredTime < time.Now().Unix()) {
		return false, ErrBillingTokenUnavailable
	}
	if quota == 0 {
		return false, nil
	}
	if !token.UnlimitedQuota && (token.RemainQuota < amount || token.UsedQuota > math.MaxInt-amount) {
		return false, ErrTokenQuotaInsufficient
	}

	if result := tx.Model(&User{}).Where("id = ?", userID).Update("quota", gorm.Expr("quota - ?", amount)); result.Error != nil || result.RowsAffected != 1 {
		if result.Error != nil {
			return false, result.Error
		}
		return false, ErrBillingUserUnavailable
	}
	if token.UnlimitedQuota {
		return false, nil
	}
	updates := map[string]any{
		"remain_quota":  gorm.Expr("remain_quota - ?", amount),
		"used_quota":    gorm.Expr("used_quota + ?", amount),
		"accessed_time": utils.GetTimestamp(),
	}
	if result := tx.Model(&Token{}).Where("id = ?", tokenID).Updates(updates); result.Error != nil || result.RowsAffected != 1 {
		if result.Error != nil {
			return false, result.Error
		}
		return false, ErrBillingTokenUnavailable
	}
	return true, nil
}

func ApplyBillingRefund(ctx context.Context, userID, tokenID int, tokenQuotaApplied bool, refund int64) (BillingBalanceResult, error) {
	if refund <= 0 {
		return BillingBalanceResult{Outcome: BillingBalanceDefinitelyRolledBack}, errors.New("billing refund must be positive")
	}
	if refund > int64(math.MaxInt) {
		return BillingBalanceResult{Outcome: BillingBalanceDefinitelyRolledBack}, ErrBillingRange
	}
	result, err := applyBillingBalanceTransaction(ctx, func(tx *gorm.DB) (bool, error) {
		return ApplyBillingRefundInTransaction(tx, userID, tokenID, tokenQuotaApplied, refund)
	})
	return result, err
}

// ApplyBillingSettlementBalances reconciles the user balance and token balance
// independently. The current token UnlimitedQuota policy is read under the
// same principal lock; preconsumeTokenApplied records what the reserve
// transaction actually changed.
func ApplyBillingSettlementBalances(ctx context.Context, userID, tokenID int, preConsumedQuota, finalQuota int64, preconsumeTokenApplied bool) (BillingBalanceResult, error) {
	if preConsumedQuota < 0 || finalQuota < 0 || preConsumedQuota > int64(math.MaxInt) || finalQuota > int64(math.MaxInt) {
		return BillingBalanceResult{Outcome: BillingBalanceDefinitelyRolledBack}, ErrBillingRange
	}
	result, err := applyBillingBalanceTransaction(ctx, func(tx *gorm.DB) (bool, error) {
		tokenApplied, _, err := applyBillingSettlementBalancesInTransaction(tx, userID, tokenID, preConsumedQuota, finalQuota, preconsumeTokenApplied)
		return tokenApplied, err
	})
	return result, err
}

func applyBillingSettlementBalancesInTransaction(tx *gorm.DB, userID, tokenID int, preConsumedQuota, finalQuota int64, preconsumeTokenApplied bool) (bool, bool, error) {
	if preConsumedQuota < 0 || finalQuota < 0 || preConsumedQuota > int64(math.MaxInt) || finalQuota > int64(math.MaxInt) {
		return false, false, ErrBillingRange
	}
	user, token, err := lockBillingPrincipal(tx, userID, tokenID, true)
	if err != nil {
		return false, false, err
	}
	userDelta := finalQuota - preConsumedQuota
	alreadyTokenCharge := int64(0)
	if preconsumeTokenApplied {
		alreadyTokenCharge = preConsumedQuota
	}
	targetTokenCharge := int64(0)
	if !token.UnlimitedQuota {
		targetTokenCharge = finalQuota
	}
	tokenDelta := targetTokenCharge - alreadyTokenCharge

	if err := validateBillingDeltaRange(user.Quota, user.UsedQuota, userDelta, false); err != nil {
		return false, false, err
	}
	if err := validateBillingDeltaRange(token.RemainQuota, token.UsedQuota, tokenDelta, true); err != nil {
		return false, false, err
	}
	userChanged, err := applyUserQuotaChange(tx.Unscoped(), &user, -userDelta, finalQuota)
	if err != nil {
		return false, false, err
	}

	if tokenDelta != 0 {
		updates := map[string]any{
			"remain_quota":  gorm.Expr("remain_quota - ?", tokenDelta),
			"used_quota":    gorm.Expr("used_quota + ?", tokenDelta),
			"accessed_time": utils.GetTimestamp(),
		}
		result := tx.Unscoped().Model(&Token{}).Where("id = ?", tokenID).Updates(updates)
		if result.Error != nil || result.RowsAffected != 1 {
			if result.Error != nil {
				return false, false, result.Error
			}
			return false, false, ErrBillingTokenUnavailable
		}
	}
	return !token.UnlimitedQuota, userChanged || tokenDelta != 0, nil
}

func validateBillingDeltaRange(balance, used int, delta int64, trackUsed bool) error {
	if delta > int64(math.MaxInt) || delta < int64(math.MinInt) {
		return ErrBillingRange
	}
	amount := int(delta)
	if amount > 0 {
		if balance < math.MinInt+amount {
			return ErrBillingRange
		}
		if trackUsed && used > math.MaxInt-amount {
			return ErrBillingRange
		}
		return nil
	}
	if amount < 0 {
		refund := -amount
		if balance > math.MaxInt-refund {
			return ErrBillingRange
		}
		if trackUsed && used < refund {
			return ErrBillingRange
		}
	}
	return nil
}

func ApplyBillingRefundInTransaction(tx *gorm.DB, userID, tokenID int, tokenQuotaApplied bool, refund int64) (bool, error) {
	if refund < 0 || refund > int64(math.MaxInt) {
		return false, ErrBillingRange
	}
	if refund == 0 {
		return false, nil
	}
	user, token, err := lockBillingPrincipal(tx, userID, tokenID, true)
	if err != nil {
		return false, err
	}
	amount := int(refund)
	if user.Quota > math.MaxInt-amount {
		return false, ErrBillingRange
	}
	if tokenQuotaApplied && (token.RemainQuota > math.MaxInt-amount || token.UsedQuota < amount) {
		return false, ErrBillingRange
	}

	if _, err := applyUserQuotaChange(tx.Unscoped(), &user, refund, 0); err != nil {
		return false, err
	}

	if !tokenQuotaApplied {
		return false, nil
	}
	updates := map[string]any{
		"remain_quota":  gorm.Expr("remain_quota + ?", amount),
		"used_quota":    gorm.Expr("used_quota - ?", amount),
		"accessed_time": utils.GetTimestamp(),
	}
	if result := tx.Unscoped().Model(&Token{}).Where("id = ?", tokenID).Updates(updates); result.Error != nil || result.RowsAffected != 1 {
		if result.Error != nil {
			return false, result.Error
		}
		return false, ErrBillingTokenUnavailable
	}
	return true, nil
}

func lockBillingPrincipal(tx *gorm.DB, userID, tokenID int, unscoped bool) (User, Token, error) {
	if tx == nil || userID <= 0 || tokenID <= 0 {
		return User{}, Token{}, ErrBillingOwnership
	}
	userQuery := tx
	if unscoped {
		userQuery = userQuery.Unscoped()
	}
	var user User
	if err := userQuery.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", userID).First(&user).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return User{}, Token{}, ErrBillingUserUnavailable
		}
		return User{}, Token{}, fmt.Errorf("lock billing user: %w", err)
	}

	tokenQuery := tx
	if unscoped {
		tokenQuery = tokenQuery.Unscoped()
	}
	var token Token
	if err := tokenQuery.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", tokenID).First(&token).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return User{}, Token{}, ErrBillingTokenUnavailable
		}
		return User{}, Token{}, fmt.Errorf("lock billing token: %w", err)
	}
	if token.UserId != userID {
		return User{}, Token{}, ErrBillingOwnership
	}
	return user, token, nil
}

func applyBillingBalanceTransaction(ctx context.Context, apply func(*gorm.DB) (bool, error)) (result BillingBalanceResult, err error) {
	if DB == nil {
		return BillingBalanceResult{Outcome: BillingBalanceDefinitelyRolledBack}, errors.New("billing database is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	tx := DB.WithContext(ctx).Begin()
	if tx.Error != nil {
		return BillingBalanceResult{Outcome: BillingBalanceDefinitelyRolledBack}, tx.Error
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			_ = tx.Rollback().Error
			panic(recovered)
		}
	}()

	tokenApplied, applyErr := apply(tx)
	if applyErr != nil {
		_ = tx.Rollback().Error
		return BillingBalanceResult{Outcome: BillingBalanceDefinitelyRolledBack}, applyErr
	}
	result.TokenQuotaApplied = tokenApplied
	result.CommitAttempted = true
	result.Outcome = BillingBalanceCommitUnknown
	if err = tx.Commit().Error; err != nil {
		return result, err
	}
	result.Outcome = BillingBalanceCommitted
	return result, nil
}
