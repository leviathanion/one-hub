package model

import (
	"context"
	"encoding/json"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"one-api/payment/types"
	"time"
)

func FindOrderRequest(ctx context.Context, userID int, key string) (*Order, error) {
	var order Order
	err := DB.WithContext(ctx).Where("user_id = ? AND request_key = ?", userID, key).First(&order).Error
	return &order, err
}
func FindPaymentOrder(ctx context.Context, tradeNo string) (*Order, error) {
	var order Order
	err := DB.WithContext(ctx).Where("trade_no = ?", tradeNo).First(&order).Error
	return &order, err
}
func PersistPaymentOrder(ctx context.Context, order *Order) (*Order, error) {
	if err := DB.WithContext(ctx).Create(order).Error; err != nil {
		existing, lookup := FindOrderRequest(ctx, order.UserId, order.RequestKey)
		if lookup == nil {
			return existing, nil
		}
		return nil, err
	}
	return order, nil
}
func ClaimOrderPreparation(ctx context.Context, id int, claim string, now, deadline time.Time, revision int64) (bool, error) {
	r := DB.WithContext(ctx).Model(&Order{}).Where("id = ? AND preparation_state = ? AND payment_state = ? AND window_state = ? AND local_display_until > ?", id, types.PreparationNew, types.PaymentUnconfirmed, types.WindowOpen, now).Updates(map[string]any{"preparation_state": types.PreparationClaimed, "preparation_claim_id": claim, "preparation_claimed_at": now, "preparation_deadline": deadline, "prepared_with_revision": revision})
	return r.RowsAffected == 1, r.Error
}
func RecoverOrderPreparation(ctx context.Context, order *Order, now time.Time) error {
	if order.PreparationState != types.PreparationClaimed || order.PreparationDeadline == nil || now.Before(*order.PreparationDeadline) {
		return nil
	}
	state := types.PreparationUnknown
	// 仅本地签名动作可以重新生成原冻结动作；远端创建永不重放。
	if order.PreparationMode == types.BrowserHandoff && order.WindowState == types.WindowOpen && now.Before(order.LocalDisplayUntil) {
		state = types.PreparationNew
	}
	return DB.WithContext(ctx).Model(&Order{}).Where("id = ? AND preparation_state = ? AND preparation_claim_id = ? AND preparation_deadline <= ?", order.ID, types.PreparationClaimed, order.PreparationClaimID, now).Updates(map[string]any{"preparation_state": state, "last_error_code": "preparation_deadline"}).Error
}
func SavePreparationResult(ctx context.Context, order *Order, claim string, result types.PrepareResult) error {
	action, err := json.Marshal(result.NextAction)
	if err != nil {
		return err
	}
	values := map[string]any{"preparation_state": result.Outcome, "next_action": string(action), "provider_payable_until": result.ProviderPayableUntil, "expiry_source": result.ExpirySource, "last_error_code": result.ErrorCode}
	// 条件回写资源归属，早到付款绑定的资源不能被迟到创建结果替换。
	if result.ProviderResourceRef != "" {
		values["provider_resource_ref"] = result.ProviderResourceRef
	}
	query := DB.WithContext(ctx).Model(&Order{}).Where("id = ? AND preparation_claim_id = ? AND preparation_state IN ?", order.ID, claim, []string{types.PreparationClaimed, types.PreparationUnknown})
	if result.ProviderResourceRef != "" {
		query = query.Where("provider_resource_ref = '' OR provider_resource_ref = ?", result.ProviderResourceRef)
	}
	r := query.Updates(values)
	if r.Error != nil {
		return r.Error
	}
	if r.RowsAffected != 1 {
		return ErrPaymentOrderConflict
	}
	return nil
}
func SavePaymentObservation(ctx context.Context, e types.PaymentObservation, now time.Time) (*Order, error) {
	var order Order
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("trade_no = ?", e.TradeNo).First(&order).Error; err != nil {
			return err
		}
		if err := order.MatchObservation(e); err != nil {
			return err
		}
		if order.PaymentState == types.PaymentPaid {
			return nil
		}
		values := map[string]any{"last_observed_at": now, "last_provider_status": e.RawStatus, "last_error_code": e.SafeDiagnostics}
		switch e.State {
		case types.ObservationProcessing:
			values["payment_state"] = types.PaymentProcessing
		case types.ObservationUnpaid, types.ObservationAttemptFailed:
			values["payment_state"] = types.PaymentUnconfirmed
		case types.ObservationClosed:
			values["window_state"] = types.WindowProviderClosed
		}
		if e.ProviderResourceRef != "" && order.ProviderResourceRef == "" {
			values["provider_resource_ref"] = e.ProviderResourceRef
		}
		r := tx.Model(&Order{}).Where("id = ? AND payment_state <> ?", order.ID, types.PaymentPaid).Updates(values)
		if r.Error != nil {
			return r.Error
		}
		return tx.First(&order, order.ID).Error
	})
	return &order, err
}
func ClaimOrderQuery(ctx context.Context, id int, now, next time.Time) (bool, error) {
	r := DB.WithContext(ctx).Model(&Order{}).Where("id = ? AND payment_state <> ? AND (next_query_at IS NULL OR next_query_at <= ?)", id, types.PaymentPaid, now).Updates(map[string]any{"next_query_at": next, "query_count": gorm.Expr("query_count + 1")})
	return r.RowsAffected == 1, r.Error
}
func SetOrderDiagnostic(ctx context.Context, id int, code string) error {
	return DB.WithContext(ctx).Model(&Order{}).Where("id = ?", id).Update("last_error_code", code).Error
}
func OrdersToReconcile(ctx context.Context, now time.Time, limit int) ([]Order, error) {
	var orders []Order
	err := DB.WithContext(ctx).Where("payment_state <> ? AND ((preparation_state = ? AND preparation_deadline <= ?) OR (created_at >= ? AND (query_by_merchant_ref = ? OR (query_by_resource_ref = ? AND provider_resource_ref <> '')) AND (next_query_at IS NULL OR next_query_at <= ?)))", types.PaymentPaid, types.PreparationClaimed, now, now.Add(-24*time.Hour).Unix(), true, true, now).Order("id ASC").Limit(limit).Find(&orders).Error
	return orders, err
}
func ClaimOrderClose(ctx context.Context, id int) (bool, error) {
	r := DB.WithContext(ctx).Model(&Order{}).Where("id = ? AND close_state = '' AND payment_state = ?", id, types.PaymentUnconfirmed).Update("close_state", "claimed")
	return r.RowsAffected == 1, r.Error
}
func FinishOrderClose(ctx context.Context, id int, state, code string) error {
	r := DB.WithContext(ctx).Model(&Order{}).Where("id = ? AND close_state = ?", id, "claimed").Updates(map[string]any{"close_state": state, "last_error_code": code})
	if r.Error != nil {
		return r.Error
	}
	if r.RowsAffected != 1 {
		return ErrPaymentOrderConflict
	}
	return nil
}
