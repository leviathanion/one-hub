package model

import (
	"context"
	"errors"
	"testing"

	"one-api/common/config"
)

func TestBillingReserveAndRefundApplyEqualUserTokenDeltas(t *testing.T) {
	useTokenSettlementTestDB(t)
	insertTokenSettlementFixtures(t)

	reserved, err := ApplyBillingReserve(context.Background(), 1, 1, 300)
	if err != nil || reserved.Outcome != BillingBalanceCommitted || !reserved.TokenQuotaApplied {
		t.Fatalf("reserve result=%+v err=%v", reserved, err)
	}
	assertBillingBalances(t, 700, 700, 300)

	refunded, err := ApplyBillingRefund(context.Background(), 1, 1, reserved.TokenQuotaApplied, 125)
	if err != nil || refunded.Outcome != BillingBalanceCommitted || !refunded.TokenQuotaApplied {
		t.Fatalf("refund result=%+v err=%v", refunded, err)
	}
	assertBillingBalances(t, 825, 825, 175)
}

func TestBillingReserveDoesNotUseBatchOrCacheAdmission(t *testing.T) {
	useTokenSettlementTestDB(t)
	insertTokenSettlementFixtures(t)

	originalBatch := config.BatchUpdateEnabled
	config.BatchUpdateEnabled = true
	resetBatchUpdateStoresForTest()
	t.Cleanup(func() {
		config.BatchUpdateEnabled = originalBatch
		resetBatchUpdateStoresForTest()
	})

	result, err := ApplyBillingReserve(context.Background(), 1, 1, 100)
	if err != nil || result.Outcome != BillingBalanceCommitted {
		t.Fatalf("reserve result=%+v err=%v", result, err)
	}
	assertBillingBalances(t, 900, 900, 100)
	for i := 0; i < BatchUpdateTypeCount; i++ {
		if len(batchUpdateStores[i]) != 0 {
			t.Fatalf("billing reserve wrote batch store %d: %+v", i, batchUpdateStores[i])
		}
	}
}

func TestIncreaseUserQuotaIsImmediatelyVisibleWhenBatchingIsEnabled(t *testing.T) {
	useTokenSettlementTestDB(t)
	if err := DB.AutoMigrate(&UserGroup{}); err != nil {
		t.Fatal(err)
	}
	insertTokenSettlementFixtures(t)
	if err := DB.Model(&User{}).Where("id = ?", 1).Update("quota", 0).Error; err != nil {
		t.Fatal(err)
	}

	originalBatch := config.BatchUpdateEnabled
	config.BatchUpdateEnabled = true
	resetBatchUpdateStoresForTest()
	t.Cleanup(func() {
		config.BatchUpdateEnabled = originalBatch
		resetBatchUpdateStoresForTest()
	})

	if err := IncreaseUserQuota(1, 100); err != nil {
		t.Fatalf("increase user quota: %v", err)
	}
	if result, err := CheckBillingAdmission(context.Background(), 1, 1); err != nil || result.Outcome != BillingBalanceCommitted {
		t.Fatalf("new quota was not immediately admissible: result=%+v err=%v", result, err)
	}
	assertBillingBalances(t, 100, 1000, 0)

	batchUpdate()
	assertBillingBalances(t, 100, 1000, 0)
}

func TestBillingReserveRejectsUnavailablePrincipalWithoutPartialDebit(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T)
		want   error
	}{
		{
			name: "disabled user",
			mutate: func(t *testing.T) {
				if err := DB.Model(&User{}).Where("id = ?", 1).Update("status", config.UserStatusDisabled).Error; err != nil {
					t.Fatal(err)
				}
			},
			want: ErrBillingUserUnavailable,
		},
		{
			name: "expired token",
			mutate: func(t *testing.T) {
				if err := DB.Model(&Token{}).Where("id = ?", 1).Update("expired_time", 1).Error; err != nil {
					t.Fatal(err)
				}
			},
			want: ErrBillingTokenUnavailable,
		},
		{
			name: "ownership mismatch",
			mutate: func(t *testing.T) {
				if err := DB.Model(&Token{}).Where("id = ?", 1).Update("user_id", 2).Error; err != nil {
					t.Fatal(err)
				}
			},
			want: ErrBillingOwnership,
		},
		{
			name: "insufficient user quota",
			mutate: func(t *testing.T) {
				if err := DB.Model(&User{}).Where("id = ?", 1).Update("quota", 50).Error; err != nil {
					t.Fatal(err)
				}
			},
			want: ErrBillingUserQuotaInsufficient,
		},
		{
			name: "insufficient token quota",
			mutate: func(t *testing.T) {
				if err := DB.Model(&Token{}).Where("id = ?", 1).Update("remain_quota", 50).Error; err != nil {
					t.Fatal(err)
				}
			},
			want: ErrTokenQuotaInsufficient,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			useTokenSettlementTestDB(t)
			insertTokenSettlementFixtures(t)
			tt.mutate(t)

			result, err := ApplyBillingReserve(context.Background(), 1, 1, 100)
			if !errors.Is(err, tt.want) || result.Outcome != BillingBalanceDefinitelyRolledBack || result.CommitAttempted {
				t.Fatalf("reserve result=%+v err=%v, want %v", result, err, tt.want)
			}
			var user User
			var token Token
			if err := DB.First(&user, 1).Error; err != nil {
				t.Fatal(err)
			}
			if err := DB.First(&token, 1).Error; err != nil {
				t.Fatal(err)
			}
			if user.UsedQuota != 0 || token.UsedQuota != 0 {
				t.Fatalf("failed reserve partially applied usage: user=%+v token=%+v", user, token)
			}
			switch tt.want {
			case ErrBillingUserQuotaInsufficient:
				if user.Quota != 50 || token.RemainQuota != 1000 {
					t.Fatalf("failed user guard changed balances: user=%+v token=%+v", user, token)
				}
			case ErrTokenQuotaInsufficient:
				if user.Quota != 1000 || token.RemainQuota != 50 {
					t.Fatalf("failed token guard changed balances: user=%+v token=%+v", user, token)
				}
			default:
				if user.Quota != 1000 || token.RemainQuota != 1000 {
					t.Fatalf("failed principal guard changed balances: user=%+v token=%+v", user, token)
				}
			}
		})
	}
}

func TestBillingRefundInvariantFailureKeepsFullReservation(t *testing.T) {
	useTokenSettlementTestDB(t)
	insertTokenSettlementFixtures(t)

	reserved, err := ApplyBillingReserve(context.Background(), 1, 1, 300)
	if err != nil {
		t.Fatal(err)
	}
	if err := DB.Model(&Token{}).Where("id = ?", 1).Update("used_quota", 10).Error; err != nil {
		t.Fatal(err)
	}

	result, err := ApplyBillingRefund(context.Background(), 1, 1, reserved.TokenQuotaApplied, 100)
	if !errors.Is(err, ErrBillingRange) || result.Outcome != BillingBalanceDefinitelyRolledBack {
		t.Fatalf("refund result=%+v err=%v", result, err)
	}
	assertBillingBalances(t, 700, 700, 10)
}

func TestBillingUnlimitedTokenOnlyChangesUserBalance(t *testing.T) {
	useTokenSettlementTestDB(t)
	insertTokenSettlementFixtures(t)
	if err := DB.Model(&Token{}).Where("id = ?", 1).Update("unlimited_quota", true).Error; err != nil {
		t.Fatal(err)
	}

	reserved, err := ApplyBillingReserve(context.Background(), 1, 1, 250)
	if err != nil || reserved.TokenQuotaApplied {
		t.Fatalf("reserve result=%+v err=%v", reserved, err)
	}
	assertBillingBalances(t, 750, 1000, 0)

	refunded, err := ApplyBillingRefund(context.Background(), 1, 1, reserved.TokenQuotaApplied, 250)
	if err != nil || refunded.TokenQuotaApplied {
		t.Fatalf("refund result=%+v err=%v", refunded, err)
	}
	assertBillingBalances(t, 1000, 1000, 0)
}

func TestBillingSettlementUsesCurrentUnlimitedPolicyAfterLimitedReserve(t *testing.T) {
	useTokenSettlementTestDB(t)
	insertTokenSettlementFixtures(t)
	reserved, err := ApplyBillingReserve(context.Background(), 1, 1, 100)
	if err != nil || !reserved.TokenQuotaApplied {
		t.Fatalf("reserve result=%+v err=%v", reserved, err)
	}
	if err := DB.Model(&Token{}).Where("id = ?", 1).Update("unlimited_quota", true).Error; err != nil {
		t.Fatal(err)
	}
	result, err := ApplyBillingSettlementBalances(context.Background(), 1, 1, 100, 300, reserved.TokenQuotaApplied)
	if err != nil || result.Outcome != BillingBalanceCommitted || result.TokenQuotaApplied {
		t.Fatalf("settlement result=%+v err=%v", result, err)
	}
	assertBillingBalances(t, 700, 1000, 0)
}

func TestBillingSettlementUsesCurrentLimitedPolicyAfterUnlimitedReserve(t *testing.T) {
	useTokenSettlementTestDB(t)
	insertTokenSettlementFixtures(t)
	if err := DB.Model(&Token{}).Where("id = ?", 1).Update("unlimited_quota", true).Error; err != nil {
		t.Fatal(err)
	}
	reserved, err := ApplyBillingReserve(context.Background(), 1, 1, 100)
	if err != nil || reserved.TokenQuotaApplied {
		t.Fatalf("reserve result=%+v err=%v", reserved, err)
	}
	if err := DB.Model(&Token{}).Where("id = ?", 1).Update("unlimited_quota", false).Error; err != nil {
		t.Fatal(err)
	}
	result, err := ApplyBillingSettlementBalances(context.Background(), 1, 1, 100, 300, reserved.TokenQuotaApplied)
	if err != nil || result.Outcome != BillingBalanceCommitted || !result.TokenQuotaApplied {
		t.Fatalf("settlement result=%+v err=%v", result, err)
	}
	assertBillingBalances(t, 700, 700, 300)
}

func TestBillingSettlementPositiveFinalDeltaMayOverdraw(t *testing.T) {
	useTokenSettlementTestDB(t)
	insertTokenSettlementFixtures(t)

	reserved, err := ApplyBillingReserve(context.Background(), 1, 1, 100)
	if err != nil {
		t.Fatal(err)
	}
	result, err := ApplyBillingSettlementBalances(context.Background(), 1, 1, 100, 1200, reserved.TokenQuotaApplied)
	if err != nil || result.Outcome != BillingBalanceCommitted {
		t.Fatalf("post-paid delta result=%+v err=%v", result, err)
	}
	assertBillingBalances(t, -200, -200, 1200)
}

func assertBillingBalances(t *testing.T, userQuota, tokenRemain, tokenUsed int) {
	t.Helper()
	var user User
	if err := DB.Unscoped().First(&user, 1).Error; err != nil {
		t.Fatal(err)
	}
	var token Token
	if err := DB.Unscoped().First(&token, 1).Error; err != nil {
		t.Fatal(err)
	}
	if user.Quota != userQuota || token.RemainQuota != tokenRemain || token.UsedQuota != tokenUsed {
		t.Fatalf("balances user=%d token=(%d,%d), want user=%d token=(%d,%d)", user.Quota, token.RemainQuota, token.UsedQuota, userQuota, tokenRemain, tokenUsed)
	}
}
