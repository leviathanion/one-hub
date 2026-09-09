package model

import (
	"context"
	"errors"
	"one-api/common/utils"
	paytypes "one-api/payment/types"

	"gorm.io/gorm"
)

type CurrencyType string

const (
	CurrencyTypeUSD CurrencyType = "USD"
	CurrencyTypeCNY CurrencyType = "CNY"
)

type Payment struct {
	ID                   int                      `json:"id"`
	Type                 string                   `json:"type" form:"type" gorm:"type:varchar(16)"`
	UUID                 string                   `json:"uuid" form:"uuid" gorm:"type:char(32);uniqueIndex"`
	Name                 string                   `json:"name" form:"name" gorm:"type:varchar(255); not null"`
	Icon                 string                   `json:"icon" form:"icon" gorm:"type:varchar(300)"`
	NotifyDomain         string                   `json:"notify_domain" form:"notify_domain" gorm:"type:varchar(300)"`
	FixedFee             float64                  `json:"fixed_fee" form:"fixed_fee" gorm:"type:decimal(10,2); default:0.00"`
	PercentFee           float64                  `json:"percent_fee" form:"percent_fee" gorm:"type:decimal(10,2); default:0.00"`
	Currency             CurrencyType             `json:"currency" form:"currency" gorm:"type:varchar(5)"`
	Config               string                   `json:"config" form:"config" gorm:"type:text"`
	Identity             paytypes.GatewayIdentity `json:"identity" gorm:"serializer:json;type:text;not null"`
	TransactionNamespace string                   `json:"transaction_namespace" gorm:"type:varchar(512);not null"`
	CredentialRevision   int64                    `json:"credential_revision" gorm:"not null;default:1"`
	DefaultMethod        string                   `json:"default_method" gorm:"type:varchar(32)"`
	DefaultProduct       string                   `json:"default_product" gorm:"type:varchar(64)"`
	SetupStatus          string                   `json:"setup_status" gorm:"type:varchar(32);not null"`
	SetupError           string                   `json:"setup_error,omitempty" gorm:"type:varchar(255)"`
	Sort                 int                      `json:"sort" form:"sort" gorm:"default:1"`
	Enable               *bool                    `json:"enable" form:"enable" gorm:"default:true"`
	CreatedAt            int64                    `json:"created_at" gorm:"bigint"`
	UpdatedAt            int64                    `json:"-" gorm:"bigint"`
	DeletedAt            gorm.DeletedAt           `json:"-" gorm:"index"`
}

type PaymentView struct {
	Payment
	ProtocolProfile string `json:"protocol_profile"`
}

func (p Payment) View() PaymentView {
	return PaymentView{Payment: p, ProtocolProfile: p.Identity.ProtocolProfile}
}

func GetPaymentByID(id int) (*Payment, error) {
	var payment Payment
	err := DB.First(&payment, id).Error
	return &payment, err
}

func GetPaymentByUUID(uuid string) (*Payment, error) {
	var payment Payment
	err := DB.Where("uuid = ? AND enable = ? AND setup_status = ?", uuid, true, "ready").First(&payment).Error
	return &payment, err
}

var allowedPaymentOrderFields = map[string]bool{
	"id":         true,
	"uuid":       true,
	"name":       true,
	"type":       true,
	"sort":       true,
	"enable":     true,
	"created_at": true,
}

type SearchPaymentParams struct {
	Payment
	PaginationParams
}

func GetPanymentList(params *SearchPaymentParams) (*DataResult[Payment], error) {
	var payments []*Payment

	db := DB.Omit("config")

	if params.Type != "" {
		db = db.Where("type = ?", params.Type)
	}

	if params.Name != "" {
		db = db.Where("name LIKE ?", params.Name+"%")
	}

	if params.UUID != "" {
		db = db.Where("uuid = ?", params.UUID)
	}

	if params.Currency != "" {
		db = db.Where("currency = ?", params.Currency)
	}

	return PaginateAndOrder(db, &params.PaginationParams, &payments, allowedPaymentOrderFields)
}

func GetUserPaymentList() ([]*Payment, error) {
	var payments []*Payment
	err := DB.Model(payments).Select("uuid, name, icon, fixed_fee, percent_fee, currency, sort, default_product").Where("enable = ? AND setup_status = ?", true, "ready").Find(&payments).Error
	return payments, err
}

func (p *Payment) Insert() error {
	p.UUID = utils.GetUUID()
	return DB.Create(p).Error
}

// 普通编辑只改变后续订单使用的业务元数据，绝不回写身份和凭证快照。
func (p *Payment) Update(_ bool) error {
	if p.ID <= 0 {
		return errors.New("支付网关不存在")
	}
	return DB.Model(&Payment{}).Where("id = ?", p.ID).Select("name", "icon", "notify_domain", "fixed_fee", "percent_fee", "currency", "sort", "enable", "default_product").Updates(p).Error
}

func GetHistoricalPaymentByUUID(uuid string) (*Payment, error) {
	var p Payment
	err := DB.Unscoped().Where("uuid = ?", uuid).First(&p).Error
	return &p, err
}
func GetHistoricalPaymentByID(id int) (*Payment, error) {
	var p Payment
	err := DB.Unscoped().First(&p, id).Error
	return &p, err
}
func (p *Payment) Snapshot() paytypes.GatewaySnapshot {
	return paytypes.GatewaySnapshot{GatewayID: p.ID, UUID: p.UUID, CredentialRevision: p.CredentialRevision, Identity: p.Identity, TransactionNamespace: p.TransactionNamespace, Config: p.Config, DefaultProduct: p.DefaultProduct}
}
func RotatePaymentCredentials(ctx context.Context, id int, revision int64, newConfig string) error {
	result := DB.WithContext(ctx).Unscoped().Model(&Payment{}).Where("id = ? AND credential_revision = ?", id, revision).Updates(map[string]any{"config": newConfig, "credential_revision": revision + 1, "setup_status": "ready", "setup_error": ""})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return errors.New("凭证版本已变化，请重新读取后再提交")
	}
	return nil
}

func (p *Payment) Delete() error {
	return DB.Delete(p).Error
}
