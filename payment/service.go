package payment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"one-api/common/config"
	"one-api/common/utils"
	"one-api/model"
	"one-api/payment/types"
)

const OperationTimeout = 30 * time.Second

var Now = func() time.Time { return time.Now().UTC() }

type PaymentService struct{ Payment *model.Payment }
type CreateOrderRequest struct {
	UUID       string `json:"uuid"`
	Amount     int    `json:"amount"`
	RequestKey string `json:"request_key"`
	Product    string `json:"product,omitempty"`
	Method     string `json:"method,omitempty"`
}
type OrderView struct {
	TradeNo           string           `json:"trade_no"`
	PreparationState  string           `json:"preparation_state"`
	PaymentState      string           `json:"payment_state"`
	WindowState       string           `json:"window_state"`
	ServerNow         time.Time        `json:"server_now"`
	LocalDisplayUntil time.Time        `json:"local_display_until"`
	OrderTotal        types.Money      `json:"order_total"`
	Quota             int64            `json:"quota"`
	NextAction        types.NextAction `json:"next_action"`
	CanQuery          bool             `json:"can_query"`
	LastErrorCode     string           `json:"last_error_code,omitempty"`
}

func NewPaymentService(id string) (*PaymentService, error) {
	p, err := model.GetPaymentByUUID(id)
	if err != nil {
		return nil, err
	}
	return &PaymentService{Payment: p}, nil
}
func NewHistoricalPaymentService(id string) (*PaymentService, error) {
	p, err := model.GetHistoricalPaymentByUUID(id)
	if err != nil {
		return nil, err
	}
	return &PaymentService{Payment: p}, nil
}
func (s *PaymentService) getNotifyURL(serverAddress string) string {
	domain := s.Payment.NotifyDomain
	if domain == "" {
		domain = serverAddress
	}
	return strings.TrimRight(domain, "/") + "/api/payment/notify/" + s.Payment.UUID
}
func getReturnURL(serverAddress string) string {
	return strings.TrimRight(serverAddress, "/") + "/panel/log"
}
func fingerprint(req CreateOrderRequest) string {
	req.RequestKey = ""
	b, _ := json.Marshal(req)
	hash := sha256.Sum256(b)
	return hex.EncodeToString(hash[:])
}
func CreateOrder(ctx context.Context, userID int, req CreateOrderRequest) (*OrderView, error) {
	req.UUID = strings.TrimSpace(req.UUID)
	req.Product = strings.TrimSpace(req.Product)
	req.Method = strings.TrimSpace(req.Method)
	if userID <= 0 || req.UUID == "" || req.RequestKey == "" || len(req.RequestKey) > 128 || strings.TrimSpace(req.RequestKey) != req.RequestKey || req.Amount <= 0 {
		return nil, errors.New("充值请求必须包含网关、正额和有效请求键")
	}
	fp := fingerprint(req)
	order, err := model.FindOrderRequest(ctx, userID, req.RequestKey)
	if err == nil {
		if order.RequestFingerprint != fp {
			return nil, model.ErrPaymentOrderConflict
		}
		return prepareExisting(ctx, order)
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	user, err := model.GetUserByIdWithContext(ctx, userID, false)
	if err != nil {
		return nil, err
	}
	if user.Status != config.UserStatusEnabled {
		return nil, errors.New("用户当前不可充值")
	}
	service, err := NewPaymentService(req.UUID)
	if err != nil {
		return nil, err
	}
	handle, err := Resources.Acquire(ctx, service.Payment.Snapshot())
	if err != nil {
		return nil, err
	}
	defer handle.Release()
	product := req.Product
	if product == "" {
		product = service.Payment.DefaultProduct
	}
	capabilities, err := validateCapabilities(handle.Client, product, string(service.Payment.Currency))
	if err != nil {
		return nil, err
	}
	quote, err := CalculateQuote(service.Payment, req.Amount)
	if err != nil {
		return nil, err
	}
	options := config.GlobalOption.RuntimeSnapshot()
	serverAddress := options.String("ServerAddress", config.ServerAddress)
	now := Now()
	method := req.Method
	if method == "" {
		method = service.Payment.DefaultMethod
	}
	order = &model.Order{UserId: userID, GatewayId: service.Payment.ID, TradeNo: utils.GenerateTradeNo(), RequestKey: req.RequestKey, RequestFingerprint: fp, TransactionNamespace: service.Payment.TransactionNamespace, Identity: service.Payment.Identity, ProductCode: product, MethodPreference: method, ExpectedAmountMinor: quote.Total.Minor, OrderCurrency: service.Payment.Currency, Amount: req.Amount, Quota: int(quote.Quota), Fee: quote.Fee, Discount: quote.Discount, QuoteDetails: quote.Details, PreparationMode: capabilities.PreparationMode, PreparationState: types.PreparationNew, PaymentState: types.PaymentUnconfirmed, WindowState: types.WindowOpen, NextAction: types.NoAction(), LocalDisplayUntil: now.Add(capabilities.DisplayWindow), CreateInput: types.CreateInput{NotifyURL: service.getNotifyURL(serverAddress), ReturnURL: getReturnURL(serverAddress), Description: options.String("SystemName", config.SystemName), CustomerEmail: user.Email}, QueryByMerchantRef: capabilities.QueryByMerchantRef, QueryByResourceRef: capabilities.QueryByResourceRef}
	order, err = model.PersistPaymentOrder(ctx, order)
	if err != nil {
		return nil, err
	}
	if order.RequestFingerprint != fp {
		return nil, model.ErrPaymentOrderConflict
	}
	return prepareExisting(ctx, order)
}
func prepareExisting(ctx context.Context, order *model.Order) (*OrderView, error) {
	if err := model.RecoverOrderPreparation(ctx, order, Now()); err != nil {
		return nil, err
	}
	var err error
	order, err = model.FindPaymentOrder(ctx, order.TradeNo)
	if err != nil {
		return nil, err
	}
	if order.PreparationState != types.PreparationNew || order.PaymentState != types.PaymentUnconfirmed || !Now().Before(order.LocalDisplayUntil) {
		return ViewOrder(order, Now()), nil
	}
	p, err := model.GetPaymentByID(order.GatewayId)
	if err != nil {
		return nil, err
	}
	if p.Enable == nil || !*p.Enable || p.SetupStatus != "ready" {
		return nil, errors.New("支付网关当前不接受新付款")
	}
	user, err := model.GetUserByIdWithContext(ctx, order.UserId, false)
	if err != nil {
		return nil, err
	}
	if user.Status != config.UserStatusEnabled {
		return nil, errors.New("用户当前不可充值")
	}
	if !p.Identity.Equal(order.Identity) || p.TransactionNamespace != order.TransactionNamespace {
		return nil, model.ErrPaymentOrderConflict
	}
	handle, err := Resources.Acquire(ctx, p.Snapshot())
	if err != nil {
		return nil, err
	}
	defer handle.Release()
	if _, err = validateCapabilities(handle.Client, order.ProductCode, string(order.OrderCurrency)); err != nil {
		return nil, err
	}
	claim := uuid.NewString()
	now := Now()
	deadline := now.Add(OperationTimeout)
	owned, err := model.ClaimOrderPreparation(ctx, order.ID, claim, now, deadline, p.CredentialRevision)
	// SQL 返回结果不明时，不用读到自己的 claim 猜测可再次发送。
	if err != nil {
		return nil, err
	}
	if !owned {
		return Status(ctx, order.UserId, order.TradeNo)
	}
	if !Now().Before(deadline) || !Now().Before(order.LocalDisplayUntil) {
		return Status(ctx, order.UserId, order.TradeNo)
	}
	operationCtx, cancel := context.WithTimeout(ctx, OperationTimeout)
	result, prepareErr := handle.Client.PreparePayment(operationCtx, order.Frozen())
	cancel()
	if result.Outcome == "" {
		result.Outcome = types.PreparationUnknown
	}
	if prepareErr != nil && result.ErrorCode == "" {
		result.ErrorCode = "prepare_failed"
	}
	if result.Outcome == types.PreparationReady {
		if err := normalizeAction(&result, order); err != nil {
			result.Outcome = types.PreparationUnknown
			result.ErrorCode = "invalid_action"
			result.NextAction = types.NoAction()
		}
	}
	if result.Outcome != types.PreparationReady && result.Outcome != types.PreparationRejected && result.Outcome != types.PreparationUnknown {
		result.Outcome = types.PreparationUnknown
		result.ErrorCode = "invalid_prepare_outcome"
	}
	// 即使浏览器已断开也保存原单准备结果；有限收尾不再触发供应商操作。
	saveCtx, saveCancel := context.WithTimeout(context.WithoutCancel(ctx), OperationTimeout)
	defer saveCancel()
	if err := model.SavePreparationResult(saveCtx, order, claim, result); err != nil {
		return nil, err
	}
	return Status(saveCtx, order.UserId, order.TradeNo)
}
func normalizeAction(result *types.PrepareResult, order *model.Order) error {
	a := &result.NextAction
	count := 0
	if a.Redirect != nil {
		count++
	}
	if a.Form != nil {
		count++
	}
	if a.QRCode != nil {
		count++
	}
	if count != 1 {
		return errors.New("支付动作必须恰好含一种内容")
	}
	validURL := func(raw string) bool {
		u, err := url.Parse(raw)
		return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.User == nil
	}
	switch a.Kind {
	case "redirect":
		if a.Redirect == nil || !validURL(a.Redirect.URL) {
			return errors.New("无效支付跳转")
		}
	case "form":
		if a.Form == nil || !validURL(a.Form.ActionURL) || (a.Form.Method != "GET" && a.Form.Method != "POST") {
			return errors.New("无效支付表单")
		}
	case "qr_code":
		if a.QRCode == nil || a.QRCode.Content == "" {
			return errors.New("无效支付二维码")
		}
	default:
		return errors.New("不支持的支付动作")
	}
	until := order.LocalDisplayUntil
	if result.ProviderPayableUntil != nil && result.ProviderPayableUntil.Before(until) {
		until = *result.ProviderPayableUntil
	}
	if a.ValidUntil != nil && a.ValidUntil.Before(until) {
		until = *a.ValidUntil
	}
	a.ValidUntil = &until
	return nil
}
func ViewOrder(order *model.Order, now time.Time) *OrderView {
	view := &OrderView{TradeNo: order.TradeNo, PreparationState: order.PreparationState, PaymentState: order.PaymentState, WindowState: order.WindowState, ServerNow: now, LocalDisplayUntil: order.LocalDisplayUntil, OrderTotal: order.Money(), Quota: int64(order.Quota), NextAction: types.NoAction(), CanQuery: order.QueryByMerchantRef || order.QueryByResourceRef && order.ProviderResourceRef != "", LastErrorCode: order.LastErrorCode}
	until := order.LocalDisplayUntil
	if order.ProviderPayableUntil != nil && order.ProviderPayableUntil.Before(until) {
		until = *order.ProviderPayableUntil
	}
	if order.NextAction.ValidUntil != nil && order.NextAction.ValidUntil.Before(until) {
		until = *order.NextAction.ValidUntil
	}
	if !now.Before(until) && view.WindowState != types.WindowProviderClosed {
		view.WindowState = types.WindowLocalExpired
	}
	if view.WindowState == types.WindowOpen && view.PreparationState == types.PreparationReady && view.PaymentState == types.PaymentUnconfirmed && order.CloseState == "" && order.NextAction.ValidUntil != nil && now.Before(until) {
		view.NextAction = order.NextAction
		view.NextAction.ValidUntil = &until
	}
	if view.PaymentState == types.PaymentPaid {
		view.CanQuery = false
	} else if (view.PreparationState == types.PreparationUnknown && !view.CanQuery) || now.Sub(time.Unix(int64(order.CreatedAt), 0)) > 24*time.Hour {
		view.LastErrorCode = "manual_review_required"
	}
	return view
}
func Status(ctx context.Context, userID int, tradeNo string) (*OrderView, error) {
	order, err := model.FindPaymentOrder(ctx, tradeNo)
	if err != nil {
		return nil, err
	}
	if order.UserId != userID {
		return nil, model.ErrPaymentOrderUnavailable
	}
	if err := model.RecoverOrderPreparation(ctx, order, Now()); err != nil {
		return nil, err
	}
	if order.PreparationState == types.PreparationClaimed && order.PreparationDeadline != nil && !Now().Before(*order.PreparationDeadline) {
		order, err = model.FindPaymentOrder(ctx, tradeNo)
		if err != nil {
			return nil, err
		}
	}
	return ViewOrder(order, Now()), nil
}
func ApplyPaymentObservation(ctx context.Context, observation types.PaymentObservation) (*model.Order, bool, error) {
	if observation.VerificationRef == "" || (observation.Source != types.SourceVerifiedCallback && observation.Source != types.SourceAuthenticatedQuery && observation.Source != types.SourceAuthenticatedSync) {
		return nil, false, model.ErrPaymentOrderConflict
	}
	if observation.State == types.ObservationSucceeded {
		order, newly, err := model.CompleteOrderPayment(ctx, observation)
		if err == nil && newly {
			model.RecordQuotaLog(order.UserId, model.LogTypeTopup, order.Quota, "", fmt.Sprintf("在线充值成功，充值积分：%d，订单应付总额：%s %s", order.Quota, order.Money().DecimalString(), order.OrderCurrency))
		}
		return order, newly, err
	}
	switch observation.State {
	case types.ObservationUnknown, types.ObservationUnpaid, types.ObservationProcessing, types.ObservationClosed, types.ObservationAttemptFailed, types.ObservationIgnored:
	default:
		return nil, false, model.ErrPaymentOrderConflict
	}
	order, err := model.SavePaymentObservation(ctx, observation, Now())
	return order, false, err
}
func (s *PaymentService) Callback(ctx context.Context, request types.CallbackRequest) (types.CallbackResponse, error) {
	handle, err := Resources.Acquire(ctx, s.Payment.Snapshot())
	if err != nil {
		return types.CallbackResponse{StatusCode: 503}, err
	}
	defer handle.Release()
	receiver, ok := handle.Client.(CallbackReceiver)
	if !ok {
		return types.CallbackResponse{StatusCode: 400}, types.ErrUnsupported
	}
	outcome := types.CallbackRejected
	result, err := receiver.VerifyNotification(ctx, request)
	if err != nil {
		return receiver.CallbackResponse(outcome), err
	}
	if result.Ignored && result.Observation == nil && result.QueryRef == nil {
		return receiver.CallbackResponse(types.CallbackIgnored), nil
	}
	observation := result.Observation

	if result.QueryRef != nil {
		querier, ok := handle.Client.(PaymentQuerier)
		if !ok {
			return receiver.CallbackResponse(types.CallbackRejected), types.ErrUnsupported
		}
		ref := *result.QueryRef
		if ref.TradeNo != "" {
			order, loadErr := model.FindPaymentOrder(ctx, ref.TradeNo)
			if loadErr != nil {
				return receiver.CallbackResponse(types.CallbackRetryableFailure), loadErr
			}
			if order.GatewayId != s.Payment.ID {
				return receiver.CallbackResponse(types.CallbackRejected), model.ErrPaymentOrderConflict
			}
			original := order.Ref()
			if ref.ProviderResourceRef != "" {
				if original.ProviderResourceRef != "" && original.ProviderResourceRef != ref.ProviderResourceRef {
					return receiver.CallbackResponse(types.CallbackRejected), model.ErrPaymentOrderConflict
				}
				original.ProviderResourceRef = ref.ProviderResourceRef
			}
			ref = original
		} else if ref.ProviderResourceRef == "" {
			return receiver.CallbackResponse(types.CallbackRejected), model.ErrPaymentOrderConflict
		}
		ref.GatewayID = s.Payment.ID
		if ref.ProductCode == "" {
			ref.ProductCode = s.Payment.DefaultProduct
		}
		queryCtx, cancel := context.WithTimeout(ctx, OperationTimeout)
		queried, queryErr := querier.QueryPayment(queryCtx, ref)
		cancel()
		if queryErr != nil {
			return receiver.CallbackResponse(types.CallbackRetryableFailure), queryErr
		}
		observation = &queried
	}

	if observation == nil || observation.GatewayID != s.Payment.ID || observation.TransactionNamespace != s.Payment.TransactionNamespace || !observation.Identity.Equal(s.Payment.Identity) {
		return receiver.CallbackResponse(types.CallbackRejected), model.ErrPaymentOrderConflict
	}
	_, newly, err := ApplyPaymentObservation(ctx, *observation)
	if err != nil {
		if errors.Is(err, model.ErrPaymentOrderConflict) {
			outcome = types.CallbackRejected
		} else {
			outcome = types.CallbackRetryableFailure
		}
		return receiver.CallbackResponse(outcome), err
	}
	outcome = types.CallbackIgnored
	if observation.State == types.ObservationSucceeded {
		outcome = types.CallbackAlreadyApplied
		if newly {
			outcome = types.CallbackApplied
		}
	}
	return receiver.CallbackResponse(outcome), nil
}
func QueryOrder(ctx context.Context, userID int, tradeNo string) (*OrderView, error) {
	order, err := model.FindPaymentOrder(ctx, tradeNo)
	if err != nil {
		return nil, err
	}
	if userID > 0 && order.UserId != userID {
		return nil, model.ErrPaymentOrderUnavailable
	}
	return queryOrder(ctx, order)
}
func queryOrder(ctx context.Context, order *model.Order) (*OrderView, error) {
	return queryOrderWithFresh(ctx, order, false)
}
func queryOrderWithFresh(ctx context.Context, order *model.Order, requireFresh bool) (*OrderView, error) {
	if order.PaymentState == types.PaymentPaid {
		return ViewOrder(order, Now()), nil
	}
	if !(order.QueryByMerchantRef || order.QueryByResourceRef && order.ProviderResourceRef != "") {
		return ViewOrder(order, Now()), types.ErrUnsupported
	}
	now := Now()
	interval := 30 * time.Second
	if now.Sub(time.Unix(int64(order.CreatedAt), 0)) > 10*time.Minute {
		interval = 5 * time.Minute
	}
	acquired, err := model.ClaimOrderQuery(ctx, order.ID, now, now.Add(interval))
	if err != nil {
		return nil, err
	}
	if !acquired {
		if requireFresh {
			return ViewOrder(order, Now()), errors.New("本单暂未取得新的查询许可，请稍后关单")
		}
		return Status(ctx, order.UserId, order.TradeNo)
	}
	p, err := model.GetHistoricalPaymentByID(order.GatewayId)
	if err != nil {
		return nil, err
	}
	handle, err := Resources.Acquire(ctx, p.Snapshot())
	if err != nil {
		return nil, err
	}
	defer handle.Release()
	querier, ok := handle.Client.(PaymentQuerier)
	if !ok {
		return nil, types.ErrUnsupported
	}
	operationCtx, cancel := context.WithTimeout(ctx, OperationTimeout)
	observation, err := querier.QueryPayment(operationCtx, order.Ref())
	cancel()
	if err != nil {
		_ = model.SetOrderDiagnostic(ctx, order.ID, "query_failed")
		return ViewOrder(order, Now()), err
	}
	if _, _, err = ApplyPaymentObservation(ctx, observation); err != nil {
		return nil, err
	}
	return Status(ctx, order.UserId, order.TradeNo)
}
func CloseOrder(ctx context.Context, userID int, tradeNo string) (*OrderView, error) {
	original, err := model.FindPaymentOrder(ctx, tradeNo)
	if err != nil {
		return nil, err
	}
	if original.UserId != userID {
		return nil, model.ErrPaymentOrderUnavailable
	}
	view, err := queryOrderWithFresh(ctx, original, true)
	if err != nil {
		return view, err
	}
	if view.PaymentState == types.PaymentPaid || view.PaymentState == types.PaymentProcessing || view.WindowState == types.WindowProviderClosed {
		return view, nil
	}
	order, err := model.FindPaymentOrder(ctx, tradeNo)
	if err != nil {
		return nil, err
	}
	if order.CloseState != "" {
		return view, errors.New("关单已提交，正在等待认证查询确认")
	}
	p, err := model.GetHistoricalPaymentByID(order.GatewayId)
	if err != nil {
		return nil, err
	}
	handle, err := Resources.Acquire(ctx, p.Snapshot())
	if err != nil {
		return nil, err
	}
	defer handle.Release()
	closer, ok := handle.Client.(PaymentCloser)
	if !ok {
		return view, types.ErrUnsupported
	}
	owned, err := model.ClaimOrderClose(ctx, order.ID)
	if err != nil {
		return nil, err
	}
	if !owned {
		return view, errors.New("关单已提交")
	}
	operationCtx, cancel := context.WithTimeout(ctx, OperationTimeout)
	result, closeErr := closer.ClosePayment(operationCtx, order.Ref())
	cancel()
	saveCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), OperationTimeout)
	defer stop()
	if closeErr != nil {
		_ = model.FinishOrderClose(saveCtx, order.ID, "unknown", "close_unknown")
		return view, closeErr
	}
	if result.State == types.ObservationClosed {
		_, err = model.SavePaymentObservation(saveCtx, types.PaymentObservation{TradeNo: order.TradeNo, GatewayID: p.ID, Identity: p.Identity, TransactionNamespace: p.TransactionNamespace, State: types.ObservationClosed, RawStatus: "closed"}, Now())
		if err != nil {
			return nil, err
		}
		if err = model.FinishOrderClose(saveCtx, order.ID, "closed", ""); err != nil {
			return nil, err
		}
	} else {
		if err = model.FinishOrderClose(saveCtx, order.ID, "unknown", "close_"+result.State); err != nil {
			return nil, err
		}
		// 已付款是关单协议明确返回的收敛信号，本次操作继续认证补齐资金事实。
		if result.State == types.ObservationSucceeded || result.State == types.ObservationProcessing {
			querier, ok := handle.Client.(PaymentQuerier)
			if !ok {
				return view, types.ErrUnsupported
			}
			queryCtx, end := context.WithTimeout(saveCtx, OperationTimeout)
			observation, queryErr := querier.QueryPayment(queryCtx, order.Ref())
			end()
			if queryErr != nil {
				return view, queryErr
			}
			if _, _, err = ApplyPaymentObservation(saveCtx, observation); err != nil {
				return nil, err
			}
		}
	}
	return Status(saveCtx, order.UserId, tradeNo)
}
func Reconcile(ctx context.Context) error {
	orders, err := model.OrdersToReconcile(ctx, Now(), 100)
	if err != nil {
		return err
	}
	for i := range orders {
		if err := ctx.Err(); err != nil {
			return err
		}
		order := &orders[i]
		if err := model.RecoverOrderPreparation(ctx, order, Now()); err != nil {
			return err
		}
		if order.QueryByMerchantRef || order.QueryByResourceRef && order.ProviderResourceRef != "" {
			if _, err := queryOrder(ctx, order); err != nil && !errors.Is(err, types.ErrUnsupported) {
				_ = model.SetOrderDiagnostic(ctx, order.ID, "query_failed")
			}
		}
	}
	return nil
}
