// Package types 定义支付协议的中立契约；不依赖 HTTP 框架、数据库或供应商 SDK。
package types

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"time"
)

type GatewayKind = string
type ProductCode = string
type MethodCode = string

type Money struct {
	Minor    int64  `json:"minor"`
	Currency string `json:"currency"`
	Exponent int    `json:"exponent"`
}
type GatewayIdentity struct {
	Kind            string `json:"kind"`
	Environment     string `json:"environment"`
	MerchantAccount string `json:"merchant_account"`
	AppBinding      string `json:"app_binding,omitempty"`
	EndpointScope   string `json:"endpoint_scope,omitempty"`
	ProtocolProfile string `json:"protocol_profile"`
}

func (i GatewayIdentity) Equal(other GatewayIdentity) bool { return i == other }
func Namespace(parts ...string) string                     { b, _ := json.Marshal(parts); return string(b) }

type GatewayDescriptor struct {
	Kind     string   `json:"kind"`
	Products []string `json:"products"`
}
type GatewayConfigInput struct {
	Config   string
	Currency string
	Product  string
}
type GatewayBinding struct {
	Identity             GatewayIdentity
	TransactionNamespace string
	DefaultProduct       string
	DefaultMethod        string
	Config               string
}
type GatewaySnapshot struct {
	GatewayID            int
	UUID                 string
	CredentialRevision   int64
	Identity             GatewayIdentity
	TransactionNamespace string
	Config               string
	DefaultProduct       string
}
type Capabilities struct {
	Currencies            []string      `json:"currencies"`
	PreparationMode       string        `json:"preparation_mode"`
	CanReceiveCallbacks   bool          `json:"can_receive_callbacks"`
	QueryByMerchantRef    bool          `json:"query_by_merchant_ref"`
	QueryByResourceRef    bool          `json:"query_by_resource_ref"`
	CanClose              bool          `json:"can_close"`
	CanConfigureCallbacks bool          `json:"can_configure_callbacks"`
	ExpiryMode            string        `json:"expiry_mode"`
	ExpiryStart           string        `json:"expiry_start,omitempty"`
	DisplayWindow         time.Duration `json:"-"`
	ActionKinds           []string      `json:"action_kinds"`
}

const (
	BrowserHandoff       = "browser_handoff"
	ServerCreate         = "server_create"
	PreparationNew       = "new"
	PreparationClaimed   = "claimed"
	PreparationReady     = "ready"
	PreparationRejected  = "rejected"
	PreparationUnknown   = "unknown"
	PaymentUnconfirmed   = "unconfirmed"
	PaymentProcessing    = "processing"
	PaymentPaid          = "paid"
	WindowOpen           = "open"
	WindowLocalExpired   = "local_expired"
	WindowProviderClosed = "provider_closed"
)

type RedirectAction struct {
	URL string `json:"url"`
}
type FormAction struct {
	Method    string            `json:"method"`
	ActionURL string            `json:"action_url"`
	Fields    map[string]string `json:"fields"`
}
type QRCodeAction struct {
	Content           string `json:"content"`
	OptionalLaunchURL string `json:"optional_launch_url,omitempty"`
}
type NextAction struct {
	Kind       string          `json:"kind"`
	Redirect   *RedirectAction `json:"redirect,omitempty"`
	Form       *FormAction     `json:"form,omitempty"`
	QRCode     *QRCodeAction   `json:"qr_code,omitempty"`
	ValidUntil *time.Time      `json:"valid_until,omitempty"`
}

func NoAction() NextAction { return NextAction{Kind: "none"} }

type CreateInput struct {
	NotifyURL     string `json:"notify_url"`
	ReturnURL     string `json:"return_url"`
	Description   string `json:"description"`
	CustomerEmail string `json:"customer_email,omitempty"`
}
type FrozenOrder struct {
	TradeNo              string
	GatewayID            int
	Identity             GatewayIdentity
	TransactionNamespace string
	ProductCode          string
	MethodPreference     string
	Total                Money
	Quota                int64
	LocalDisplayUntil    time.Time
	Input                CreateInput
}
type OrderRef struct {
	TradeNo               string
	GatewayID             int
	ProductCode           string
	ProviderResourceRef   string
	ProviderTransactionID string
}
type PrepareResult struct {
	Outcome              string
	NextAction           NextAction
	ProviderResourceRef  string
	ProviderPayableUntil *time.Time
	ExpirySource         string
	ErrorCode            string
}
type PaymentObservation struct {
	Source                string          `json:"source"`
	GatewayID             int             `json:"gateway_id"`
	TransactionNamespace  string          `json:"transaction_namespace"`
	Identity              GatewayIdentity `json:"identity"`
	TradeNo               string          `json:"trade_no"`
	ProviderResourceRef   string          `json:"provider_resource_ref,omitempty"`
	State                 string          `json:"state"`
	ProviderTransactionID string          `json:"provider_transaction_id,omitempty"`
	OrderTotal            *Money          `json:"order_total,omitempty"`
	MethodCode            string          `json:"method_code,omitempty"`
	ProviderEventID       string          `json:"provider_event_id,omitempty"`
	ProviderPaidAt        *time.Time      `json:"provider_paid_at,omitempty"`
	VerificationRef       string          `json:"verification_ref"`
	RawStatus             string          `json:"raw_status,omitempty"`
	SafeDiagnostics       string          `json:"safe_diagnostics,omitempty"`
}

const (
	SourceVerifiedCallback   = "verified_callback"
	SourceAuthenticatedQuery = "authenticated_query"
	SourceAuthenticatedSync  = "authenticated_sync"
	ObservationUnknown       = "unknown"
	ObservationUnpaid        = "unpaid"
	ObservationProcessing    = "processing"
	ObservationSucceeded     = "succeeded"
	ObservationClosed        = "closed"
	ObservationAttemptFailed = "attempt_failed"
	ObservationIgnored       = "ignored"
)

type CallbackRequest struct {
	Method  string
	Query   url.Values
	Headers http.Header
	Body    []byte
}
type NotificationResult struct {
	Observation *PaymentObservation
	QueryRef    *OrderRef
	Ignored     bool
}
type CallbackOutcome string

const (
	CallbackApplied          CallbackOutcome = "applied"
	CallbackAlreadyApplied   CallbackOutcome = "already_applied"
	CallbackIgnored          CallbackOutcome = "ignored"
	CallbackRetryableFailure CallbackOutcome = "retryable_failure"
	CallbackRejected         CallbackOutcome = "rejected"
)

type CallbackResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}
type CloseResult struct {
	State     string
	ErrorCode string
}
type CallbackEndpoint struct{ URL string }
type SetupResult struct {
	Config    string
	Ready     bool
	ErrorCode string
}

var ErrUnsupported = errors.New("支付产品不支持此操作")

// 接口使用中立数据；payment 重导出这些接口用于注册，adapter 无需反向导入 service。
type GatewayFactory interface {
	Descriptor() GatewayDescriptor
	ValidateConfig(context.Context, GatewayConfigInput) (GatewayBinding, error)
	NewClient(context.Context, GatewaySnapshot) (GatewayClient, error)
}
type GatewayClient interface {
	Capabilities(ProductCode) (Capabilities, error)
	PreparePayment(context.Context, FrozenOrder) (PrepareResult, error)
}
type CallbackReceiver interface {
	VerifyNotification(context.Context, CallbackRequest) (NotificationResult, error)
	CallbackResponse(CallbackOutcome) CallbackResponse
}
type PaymentQuerier interface {
	QueryPayment(context.Context, OrderRef) (PaymentObservation, error)
}
type PaymentCloser interface {
	ClosePayment(context.Context, OrderRef) (CloseResult, error)
}
type CallbackConfigurer interface {
	ConfigureCallbacks(context.Context, CallbackEndpoint) (SetupResult, error)
}
type GatewayResourceCloser interface{ CloseResources() error }
