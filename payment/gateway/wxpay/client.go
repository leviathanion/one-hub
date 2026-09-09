package wxpay

import (
	"context"
	"errors"
	"net/http"
	"time"

	"one-api/payment/types"

	"github.com/wechatpay-apiv3/wechatpay-go/core"
	"github.com/wechatpay-apiv3/wechatpay-go/services/payments"
	"github.com/wechatpay-apiv3/wechatpay-go/services/payments/native"
)

func (c *Client) PreparePayment(ctx context.Context, order types.FrozenOrder) (types.PrepareResult, error) {
	if _, err := c.Capabilities(order.ProductCode); err != nil {
		return types.PrepareResult{Outcome: types.PreparationRejected}, err
	}
	if order.GatewayID != c.snapshot.GatewayID || !order.Identity.Equal(c.snapshot.Identity) || order.TransactionNamespace != c.snapshot.TransactionNamespace || order.MethodPreference != "" && order.MethodPreference != "wxpay" || order.Total.Currency != "CNY" || order.Total.Exponent != 2 || order.Total.Minor <= 0 || order.TradeNo == "" || !order.LocalDisplayUntil.After(time.Now()) {
		return types.PrepareResult{Outcome: types.PreparationRejected}, errors.New("微信支付冻结订单无效")
	}
	service := native.NativeApiService{Client: c.api}
	response, _, err := service.Prepay(ctx, native.PrepayRequest{
		Appid: core.String(c.config.AppID), Mchid: core.String(c.config.MchID), OutTradeNo: core.String(order.TradeNo),
		Description: core.String(order.Input.Description), NotifyUrl: core.String(order.Input.NotifyURL), TimeExpire: &order.LocalDisplayUntil,
		Amount: &native.Amount{Total: core.Int64(order.Total.Minor), Currency: core.String(order.Total.Currency)},
	})
	if err != nil {
		outcome := types.PreparationUnknown
		var apiErr *core.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode >= 400 && apiErr.StatusCode < 500 && apiErr.StatusCode != http.StatusRequestTimeout && apiErr.StatusCode != http.StatusTooManyRequests {
			outcome = types.PreparationRejected
		}
		return types.PrepareResult{Outcome: outcome, ErrorCode: "wxpay_prepare_failed"}, err
	}
	if response.CodeUrl == nil || *response.CodeUrl == "" {
		return types.PrepareResult{Outcome: types.PreparationUnknown, ErrorCode: "wxpay_missing_code_url"}, errors.New("微信预下单未返回二维码")
	}
	return types.PrepareResult{Outcome: types.PreparationReady, NextAction: types.NextAction{Kind: "qr_code", QRCode: &types.QRCodeAction{Content: *response.CodeUrl}, ValidUntil: &order.LocalDisplayUntil}, ProviderPayableUntil: &order.LocalDisplayUntil, ExpirySource: "provider_time_expire"}, nil
}

func (c *Client) QueryPayment(ctx context.Context, ref types.OrderRef) (types.PaymentObservation, error) {
	if _, err := c.Capabilities(ref.ProductCode); err != nil {
		return types.PaymentObservation{}, err
	}
	if ref.GatewayID != c.snapshot.GatewayID || ref.TradeNo == "" {
		return types.PaymentObservation{}, errors.New("微信查询订单归属无效")
	}
	service := native.NativeApiService{Client: c.api}
	var transaction *payments.Transaction
	var result *core.APIResult
	var err error
	if ref.ProviderTransactionID != "" {
		transaction, result, err = service.QueryOrderById(ctx, native.QueryOrderByIdRequest{Mchid: core.String(c.config.MchID), TransactionId: core.String(ref.ProviderTransactionID)})
	} else {
		transaction, result, err = service.QueryOrderByOutTradeNo(ctx, native.QueryOrderByOutTradeNoRequest{Mchid: core.String(c.config.MchID), OutTradeNo: core.String(ref.TradeNo)})
	}
	if err != nil {
		return types.PaymentObservation{}, err
	}
	observation, err := c.observe(transaction, types.SourceAuthenticatedQuery, result.Response.Header.Get("Wechatpay-Serial"))
	if err != nil {
		return observation, err
	}
	if observation.TradeNo != ref.TradeNo || (ref.ProviderTransactionID != "" && observation.ProviderTransactionID != ref.ProviderTransactionID) {
		return observation, errors.New("微信查询返回了其他订单")
	}
	return observation, nil
}

func (c *Client) ClosePayment(ctx context.Context, ref types.OrderRef) (types.CloseResult, error) {
	if _, err := c.Capabilities(ref.ProductCode); err != nil {
		return types.CloseResult{}, err
	}
	if ref.GatewayID != c.snapshot.GatewayID || ref.TradeNo == "" {
		return types.CloseResult{}, errors.New("微信关单归属无效")
	}
	service := native.NativeApiService{Client: c.api}
	_, err := service.CloseOrder(ctx, native.CloseOrderRequest{Mchid: core.String(c.config.MchID), OutTradeNo: core.String(ref.TradeNo)})
	if err != nil {
		return types.CloseResult{}, err
	}
	return types.CloseResult{State: types.ObservationClosed}, nil
}
