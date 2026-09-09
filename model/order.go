package model

import (
	"context"
	"encoding/json"
	"errors"
	"gorm.io/gorm/clause"
	"math"
	paytypes "one-api/payment/types"
	"strings"
	"time"

	"gorm.io/gorm"
)

type OrderStatus string

const (
	OrderStatusPending OrderStatus = "pending"
	OrderStatusSuccess OrderStatus = "success"
	OrderStatusFailed  OrderStatus = "failed"
	OrderStatusClosed  OrderStatus = "closed"
)

type Order struct {
	ID                    int                      `json:"id"`
	UserId                int                      `json:"user_id" gorm:"uniqueIndex:idx_order_request"`
	GatewayId             int                      `json:"gateway_id"`
	TradeNo               string                   `json:"trade_no" gorm:"type:varchar(50);uniqueIndex"`
	Amount                int                      `json:"amount" gorm:"default:0"`
	OrderCurrency         CurrencyType             `json:"order_currency" gorm:"type:varchar(16)"`
	Quota                 int                      `json:"quota" gorm:"type:int;default:0"`
	Fee                   float64                  `json:"fee" gorm:"type:decimal(10,2);default:0"`
	Discount              float64                  `json:"discount" gorm:"type:decimal(10,2);default:0"`
	RequestKey            string                   `json:"-" gorm:"type:varchar(128);not null;uniqueIndex:idx_order_request"`
	RequestFingerprint    string                   `json:"-" gorm:"type:char(64);not null"`
	TransactionNamespace  string                   `json:"transaction_namespace" gorm:"type:varchar(512);not null;uniqueIndex:idx_order_transaction"`
	Identity              paytypes.GatewayIdentity `json:"-" gorm:"serializer:json;type:text;not null"`
	ProductCode           string                   `json:"product_code" gorm:"type:varchar(64);not null"`
	MethodPreference      string                   `json:"method_preference,omitempty" gorm:"type:varchar(32)"`
	ExpectedAmountMinor   int64                    `json:"expected_amount_minor" gorm:"not null"`
	QuoteDetails          string                   `json:"-" gorm:"type:text"`
	PreparationMode       string                   `json:"preparation_mode" gorm:"type:varchar(32);not null"`
	PreparationState      string                   `json:"preparation_state" gorm:"type:varchar(32);not null;index"`
	PreparationClaimID    string                   `json:"-" gorm:"type:varchar(64)"`
	PreparationClaimedAt  *time.Time               `json:"-"`
	PreparationDeadline   *time.Time               `json:"-" gorm:"index"`
	PreparedWithRevision  int64                    `json:"prepared_with_revision"`
	CreateInput           paytypes.CreateInput     `json:"-" gorm:"serializer:json;type:text"`
	NextAction            paytypes.NextAction      `json:"-" gorm:"serializer:json;type:text"`
	ProviderResourceRef   string                   `json:"provider_resource_ref,omitempty" gorm:"type:varchar(255)"`
	LocalDisplayUntil     time.Time                `json:"local_display_until" gorm:"not null"`
	ProviderPayableUntil  *time.Time               `json:"provider_payable_until,omitempty"`
	ExpirySource          string                   `json:"expiry_source,omitempty"`
	WindowState           string                   `json:"window_state" gorm:"type:varchar(32);not null"`
	PaymentState          string                   `json:"payment_state" gorm:"type:varchar(32);not null;check:chk_order_paid,payment_state <> 'paid' OR (provider_transaction_id IS NOT NULL AND provider_transaction_id <> '' AND expected_amount_minor > 0 AND confirmed_amount_minor IS NOT NULL AND confirmed_amount_minor = expected_amount_minor AND confirmed_currency IS NOT NULL AND order_currency IS NOT NULL AND confirmed_currency = order_currency AND confirmed_currency <> '' AND quota > 0)"`
	ProviderTransactionID *string                  `json:"provider_transaction_id,omitempty" gorm:"type:varchar(255);uniqueIndex:idx_order_transaction"`
	ConfirmedAmountMinor  *int64                   `json:"confirmed_amount_minor,omitempty"`
	ConfirmedCurrency     string                   `json:"confirmed_currency,omitempty" gorm:"type:varchar(5)"`
	EvidenceSummary       string                   `json:"-" gorm:"type:text"`
	CloseState            string                   `json:"close_state,omitempty" gorm:"type:varchar(32)"`
	QueryByMerchantRef    bool                     `json:"-"`
	QueryByResourceRef    bool                     `json:"-"`
	NextQueryAt           *time.Time               `json:"next_query_at,omitempty" gorm:"index"`
	QueryCount            int                      `json:"query_count"`
	LastObservedAt        *time.Time               `json:"last_observed_at,omitempty"`
	LastProviderStatus    string                   `json:"last_provider_status,omitempty"`
	LastErrorCode         string                   `json:"last_error_code,omitempty"`

	CreatedAt int            `json:"created_at"`
	UpdatedAt int            `json:"-"`
	DeletedAt gorm.DeletedAt `json:"-" gorm:"index"`
}

var (
	ErrPaymentOrderUnavailable = errors.New("payment order is unavailable")
	ErrPaymentOrderConflict    = errors.New("payment order state conflicts with callback")
)

func GetUserOrder(userId int, tradeNo string) (*Order, error) {
	var order Order
	err := DB.Where("user_id = ? AND trade_no = ?", userId, tradeNo).First(&order).Error
	return &order, err
}

func (o *Order) Insert() error {
	return DB.Create(o).Error
}

// CompleteOrderPayment 是在线充值唯一的资金事务入口；证据只能来自绑定适配器。
func CompleteOrderPayment(ctx context.Context, evidence paytypes.PaymentObservation) (*Order, bool, error) {
	if evidence.State != paytypes.ObservationSucceeded || evidence.OrderTotal == nil || evidence.OrderTotal.Validate() != nil || strings.TrimSpace(evidence.ProviderTransactionID) == "" || evidence.VerificationRef == "" {
		return nil, false, ErrPaymentOrderConflict
	}
	if evidence.Source != paytypes.SourceVerifiedCallback && evidence.Source != paytypes.SourceAuthenticatedQuery && evidence.Source != paytypes.SourceAuthenticatedSync {
		return nil, false, ErrPaymentOrderConflict
	}
	var order Order
	newly := false
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("trade_no = ?", evidence.TradeNo).First(&order).Error; err != nil {
			return err
		}
		if err := order.MatchObservation(evidence); err != nil {
			return err
		}
		total := order.Money()
		if *evidence.OrderTotal != total || order.Quota <= 0 {
			return ErrPaymentOrderConflict
		}
		if order.PaymentState == paytypes.PaymentPaid {
			if order.ProviderTransactionID != nil && *order.ProviderTransactionID == evidence.ProviderTransactionID && order.ConfirmedAmountMinor != nil && *order.ConfirmedAmountMinor == total.Minor && order.ConfirmedCurrency == total.Currency {
				return nil
			}
			return ErrPaymentOrderConflict
		}
		summary, _ := json.Marshal(evidence)
		now := time.Now().UTC()
		update := map[string]any{"payment_state": paytypes.PaymentPaid, "provider_transaction_id": evidence.ProviderTransactionID, "confirmed_amount_minor": total.Minor, "confirmed_currency": total.Currency, "evidence_summary": string(summary), "last_observed_at": now, "last_provider_status": evidence.RawStatus, "last_error_code": ""}
		if order.ProviderResourceRef == "" && evidence.ProviderResourceRef != "" {
			update["provider_resource_ref"] = evidence.ProviderResourceRef
		}
		result := tx.Model(&Order{}).Where("id = ? AND payment_state <> ?", order.ID, paytypes.PaymentPaid).Updates(update)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrPaymentOrderConflict
		}
		if _, err := CreditUserRecharge(tx, order.UserId, int64(order.Quota)); err != nil {
			return err
		}
		newly = true
		return tx.First(&order, order.ID).Error
	})
	if err != nil {
		// 结果不明只读取原单，绝不重放用户余额命令。
		var confirmed Order
		readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if readErr := DB.WithContext(readCtx).Where("trade_no = ?", evidence.TradeNo).First(&confirmed).Error; readErr == nil && confirmed.MatchObservation(evidence) == nil && confirmed.PaymentState == paytypes.PaymentPaid && confirmed.ProviderTransactionID != nil && *confirmed.ProviderTransactionID == evidence.ProviderTransactionID && confirmed.ConfirmedAmountMinor != nil && *confirmed.ConfirmedAmountMinor == evidence.OrderTotal.Minor && confirmed.ConfirmedCurrency == evidence.OrderTotal.Currency {

			return &confirmed, false, nil
		}
		if IsUniqueConstraintError(err) {
			var owner Order
			if lookupErr := DB.WithContext(readCtx).Where("transaction_namespace = ? AND provider_transaction_id = ?", evidence.TransactionNamespace, evidence.ProviderTransactionID).First(&owner).Error; lookupErr == nil && owner.TradeNo != evidence.TradeNo {
				return nil, false, ErrPaymentOrderConflict
			}
		}
		return nil, false, err
	}
	return &order, newly, nil
}

func (o *Order) Money() paytypes.Money {
	exponent, _ := paytypes.CurrencyExponent(string(o.OrderCurrency))
	return paytypes.Money{Minor: o.ExpectedAmountMinor, Currency: string(o.OrderCurrency), Exponent: exponent}
}

// 旧状态是付款事实、持久窗口和准备结果的展示投影；本地关闭不证明上游关单。
func (o Order) PublicStatus() OrderStatus {
	if o.PaymentState == paytypes.PaymentPaid {
		return OrderStatusSuccess
	}
	if o.WindowState == paytypes.WindowLocalExpired || o.WindowState == paytypes.WindowProviderClosed {
		return OrderStatusClosed
	}
	if o.PreparationState == paytypes.PreparationRejected {
		return OrderStatusFailed
	}
	return OrderStatusPending
}

func orderPublicStatusExpression() clause.Expr {
	return gorm.Expr("CASE WHEN payment_state = ? THEN ? WHEN window_state IN (?, ?) THEN ? WHEN preparation_state = ? THEN ? ELSE ? END",
		paytypes.PaymentPaid, OrderStatusSuccess, paytypes.WindowLocalExpired, paytypes.WindowProviderClosed, OrderStatusClosed,
		paytypes.PreparationRejected, OrderStatusFailed, OrderStatusPending)
}

// 旧 API 字段仅在输出时派生，不再作为资金事实存储。
func (o Order) MarshalJSON() ([]byte, error) {
	type storedOrder Order
	money := o.Money()
	if _, err := paytypes.CurrencyExponent(money.Currency); err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		storedOrder
		GatewayNo        string      `json:"gateway_no"`
		OrderAmount      float64     `json:"order_amount"`
		CurrencyExponent int         `json:"currency_exponent"`
		Status           OrderStatus `json:"status"`
	}{storedOrder(o), o.Ref().ProviderTransactionID, float64(money.Minor) / math.Pow10(money.Exponent), money.Exponent, o.PublicStatus()})
}
func (o *Order) Frozen() paytypes.FrozenOrder {
	return paytypes.FrozenOrder{TradeNo: o.TradeNo, GatewayID: o.GatewayId, Identity: o.Identity, TransactionNamespace: o.TransactionNamespace, ProductCode: o.ProductCode, MethodPreference: o.MethodPreference, Total: o.Money(), Quota: int64(o.Quota), LocalDisplayUntil: o.LocalDisplayUntil, Input: o.CreateInput}
}
func (o *Order) Ref() paytypes.OrderRef {
	ref := paytypes.OrderRef{TradeNo: o.TradeNo, GatewayID: o.GatewayId, ProductCode: o.ProductCode, ProviderResourceRef: o.ProviderResourceRef}
	if o.ProviderTransactionID != nil {
		ref.ProviderTransactionID = *o.ProviderTransactionID
	}
	return ref
}
func (o *Order) MatchObservation(e paytypes.PaymentObservation) error {
	if o.TradeNo != e.TradeNo || o.GatewayId != e.GatewayID || o.TransactionNamespace == "" || o.TransactionNamespace != e.TransactionNamespace || !o.Identity.Equal(e.Identity) {
		return ErrPaymentOrderConflict
	}
	if o.ProviderResourceRef != "" && e.ProviderResourceRef != "" && o.ProviderResourceRef != e.ProviderResourceRef {
		return ErrPaymentOrderConflict
	}
	return nil
}

var allowedOrderFields = map[string]bool{
	"id":         true,
	"gateway_id": true,
	"user_id":    true,
	"status":     true,
	"created_at": true,
}

type SearchOrderParams struct {
	UserId         int    `form:"user_id"`
	GatewayId      int    `form:"gateway_id"`
	TradeNo        string `form:"trade_no"`
	GatewayNo      string `form:"gateway_no"`
	Status         string `form:"status"`
	StartTimestamp int64  `form:"start_timestamp"`
	EndTimestamp   int64  `form:"end_timestamp"`
	PaginationParams
}

func GetOrderList(params *SearchOrderParams) (*DataResult[Order], error) {
	var orders []*Order

	db := DB.Select("orders.*, ? AS status", orderPublicStatusExpression())
	if params.GatewayId != 0 {
		db = db.Where("gateway_id = ?", params.GatewayId)
	}
	if params.UserId != 0 {
		db = db.Where("user_id = ?", params.UserId)
	}

	if params.TradeNo != "" {
		db = db.Where("trade_no = ?", params.TradeNo)
	}

	if params.GatewayNo != "" {
		db = db.Where("provider_transaction_id = ?", params.GatewayNo)
	}

	if params.Status != "" {
		db = db.Where("(?) = ?", orderPublicStatusExpression(), params.Status)
	}

	if params.StartTimestamp != 0 {
		db = db.Where("created_at >= ?", params.StartTimestamp)
	}
	if params.EndTimestamp != 0 {
		db = db.Where("created_at <= ?", params.EndTimestamp)
	}

	return PaginateAndOrder(db, &params.PaginationParams, &orders, allowedOrderFields)
}

type OrderStatistics struct {
	Quota         int64   `json:"quota"`
	Money         float64 `json:"money"`
	OrderCurrency string  `json:"order_currency"`
}

func GetStatisticsOrder() (orderStatistics []*OrderStatistics, err error) {
	var totals []struct {
		Quota         int64
		Minor         int64
		OrderCurrency CurrencyType
	}
	err = DB.Model(&Order{}).Select("sum(quota) as quota, sum(expected_amount_minor) as minor, order_currency").Where("payment_state = ?", paytypes.PaymentPaid).Group("order_currency").Scan(&totals).Error
	if err != nil {
		return nil, err
	}
	for _, total := range totals {
		exponent, err := paytypes.CurrencyExponent(string(total.OrderCurrency))
		if err != nil {
			return nil, err
		}
		orderStatistics = append(orderStatistics, &OrderStatistics{Quota: total.Quota, Money: float64(total.Minor) / math.Pow10(exponent), OrderCurrency: string(total.OrderCurrency)})
	}
	return orderStatistics, err
}

type OrderStatisticsGroup struct {
	Date        string  `json:"date"`
	OrderAmount float64 `json:"order_amount"`
}

func GetStatisticsOrderByPeriod(startTimestamp, endTimestamp int64) (orderStatistics []*OrderStatisticsGroup, err error) {
	groupSelect := getTimestampGroupsSelect("created_at", "day", "date")
	var totals []struct {
		Date          string
		Minor         int64
		OrderCurrency CurrencyType
	}
	err = DB.Raw(`
		SELECT `+groupSelect+`,
		sum(expected_amount_minor) as minor, order_currency
		FROM orders
		WHERE payment_state = ?
		AND created_at BETWEEN ? AND ?
		GROUP BY date, order_currency
		ORDER BY date
	`, paytypes.PaymentPaid, startTimestamp, endTimestamp).Scan(&totals).Error
	if err != nil {
		return nil, err
	}
	for _, total := range totals {
		exponent, err := paytypes.CurrencyExponent(string(total.OrderCurrency))
		if err != nil {
			return nil, err
		}
		// 保留原按日汇总 API 的口径，先按币种以整数求和再转换展示单位。
		if len(orderStatistics) == 0 || orderStatistics[len(orderStatistics)-1].Date != total.Date {
			orderStatistics = append(orderStatistics, &OrderStatisticsGroup{Date: total.Date})
		}
		orderStatistics[len(orderStatistics)-1].OrderAmount += float64(total.Minor) / math.Pow10(exponent)
	}
	return orderStatistics, err
}
