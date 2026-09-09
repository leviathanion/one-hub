package alipay

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"one-api/payment/types"
	"time"

	sdk "github.com/smartwalle/alipay/v3"
)

func rejected(err error) (types.PrepareResult, error) {
	return types.PrepareResult{Outcome: types.PreparationRejected, NextAction: types.NoAction(), ErrorCode: "invalid_order"}, err
}

func (g *gateway) PreparePayment(ctx context.Context, order types.FrozenOrder) (types.PrepareResult, error) {
	if _, err := g.Capabilities(order.ProductCode); err != nil {
		return rejected(err)
	}
	if order.GatewayID != g.snapshot.GatewayID || !order.Identity.Equal(g.snapshot.Identity) || order.TransactionNamespace != g.snapshot.TransactionNamespace {
		return rejected(errors.New("支付宝订单身份不符"))
	}
	if err := order.Total.Validate(); err != nil {
		return rejected(err)
	}
	if order.Total.Currency != "CNY" || order.TradeNo == "" || order.LocalDisplayUntil.IsZero() {
		return rejected(errors.New("无效的支付宝订单"))
	}
	if order.MethodPreference != "" && order.MethodPreference != "alipay" {
		return rejected(errors.New("支付宝不支持指定的付款方式"))
	}
	until := order.LocalDisplayUntil.Truncate(time.Second)
	if order.ProductCode == ProductWapPay {
		until = until.Truncate(time.Minute)
	}
	trade := sdk.Trade{OutTradeNo: order.TradeNo, TotalAmount: order.Total.DecimalString(), Subject: order.Input.Description, NotifyURL: order.Input.NotifyURL, ReturnURL: order.Input.ReturnURL, SellerId: g.snapshot.Identity.MerchantAccount, TimeExpire: until.In(chinaTime).Format("2006-01-02 15:04:05")}
	result := types.PrepareResult{Outcome: types.PreparationReady, ProviderPayableUntil: &until, ExpirySource: "time_expire", NextAction: types.NextAction{ValidUntil: &until}}
	var actionURL *url.URL
	var err error
	switch order.ProductCode {
	case ProductFacePay:
		response, createErr := g.client.TradePreCreate(ctx, sdk.TradePreCreate{Trade: trade})
		if createErr != nil {
			return types.PrepareResult{Outcome: types.PreparationUnknown, NextAction: types.NoAction(), ErrorCode: "precreate_unconfirmed"}, fmt.Errorf("支付宝预下单结果不明: %w", createErr)
		}
		if response == nil {
			return types.PrepareResult{Outcome: types.PreparationUnknown, NextAction: types.NoAction(), ErrorCode: "invalid_precreate_response"}, errors.New("支付宝预下单响应为空")
		}
		if !response.IsSuccess() {
			outcome := types.PreparationRejected
			if response.Code == "20000" || response.Code == "10003" {
				outcome = types.PreparationUnknown
			}
			return types.PrepareResult{Outcome: outcome, NextAction: types.NoAction(), ErrorCode: response.SubCode}, fmt.Errorf("支付宝预下单未完成: %s", response.Code)
		}
		if response.OutTradeNo != order.TradeNo || response.QRCode == "" {
			return types.PrepareResult{Outcome: types.PreparationUnknown, NextAction: types.NoAction(), ErrorCode: "invalid_precreate_response"}, errors.New("支付宝预下单响应缺少有效订单动作")
		}
		result.NextAction.Kind = "qr_code"
		result.NextAction.QRCode = &types.QRCodeAction{Content: response.QRCode}
		return result, nil
	case ProductPagePay:
		trade.ProductCode = "FAST_INSTANT_TRADE_PAY"
		actionURL, err = g.client.TradePagePay(sdk.TradePagePay{Trade: trade})
	case ProductWapPay:
		trade.ProductCode = "QUICK_WAP_WAY"
		// WAP 的具名字段覆盖嵌入 Trade.TimeExpire，且协议精度为分钟。
		actionURL, err = g.client.TradeWapPay(sdk.TradeWapPay{Trade: trade, TimeExpire: until.In(chinaTime).Format("2006-01-02 15:04")})
	}
	if err != nil {
		return rejected(err)
	}
	fields := make(map[string]string)
	for key, values := range actionURL.Query() {
		fields[key] = values[0]
	}
	actionURL.RawQuery = ""
	result.NextAction.Kind = "form"
	result.NextAction.Form = &types.FormAction{Method: http.MethodGet, ActionURL: actionURL.String(), Fields: fields}
	return result, nil
}

func (g *gateway) validateRef(ref types.OrderRef) error {
	if _, err := g.Capabilities(ref.ProductCode); err != nil {
		return err
	}
	if ref.GatewayID != g.snapshot.GatewayID || ref.TradeNo == "" {
		return errors.New("支付宝查询或关单引用不属于当前绑定")
	}
	return nil
}

func (g *gateway) QueryPayment(ctx context.Context, ref types.OrderRef) (types.PaymentObservation, error) {
	if err := g.validateRef(ref); err != nil {
		return types.PaymentObservation{}, err
	}
	// 此 profile 仅支持直连自有应用，不发送 app_auth_token；查询由绑定应用签名，
	// SDK AuxParam.NeedVerify() 为 true，平台验签后的响应才进入统一观察入口。
	var response struct {
		sdk.TradeQueryRsp
		SellerID string `json:"seller_id"`
		AppID    string `json:"app_id"`
	}
	err := g.client.Request(ctx, sdk.TradeQuery{OutTradeNo: ref.TradeNo, TradeNo: ref.ProviderTransactionID}, &response)
	if err != nil {
		return types.PaymentObservation{}, err
	}
	if !response.IsSuccess() {
		if response.SubCode == "ACQ.TRADE_NOT_EXIST" {
			return types.PaymentObservation{Source: types.SourceAuthenticatedQuery, GatewayID: g.snapshot.GatewayID, Identity: g.snapshot.Identity, TransactionNamespace: g.snapshot.TransactionNamespace, TradeNo: ref.TradeNo, State: types.ObservationUnknown, VerificationRef: fmt.Sprintf("%s/revision:%d", Profile, g.snapshot.CredentialRevision), RawStatus: response.SubCode}, nil
		}
		return types.PaymentObservation{}, fmt.Errorf("支付宝查单失败: %s", response.Code)
	}
	if response.OutTradeNo != ref.TradeNo || (ref.ProviderTransactionID != "" && response.TradeNo != ref.ProviderTransactionID) {
		return types.PaymentObservation{}, errors.New("支付宝查单交易引用不符")
	}
	if (response.SellerID != "" && response.SellerID != g.snapshot.Identity.MerchantAccount) || (response.AppID != "" && response.AppID != g.snapshot.Identity.AppBinding) {
		return types.PaymentObservation{}, errors.New("支付宝查单主体不符")
	}
	if response.TransCurrency != "" && response.TransCurrency != "CNY" {
		return types.PaymentObservation{}, errors.New("支付宝查单币种不符")
	}
	observation, err := g.observation(types.SourceAuthenticatedQuery, response.OutTradeNo, response.TradeNo, string(response.TradeStatus), response.TotalAmount, response.BuyerPayAmount, response.ReceiptAmount)
	if err != nil {
		return types.PaymentObservation{}, err
	}
	if paid, err := time.ParseInLocation("2006-01-02 15:04:05", response.SendPayDate, chinaTime); err == nil {
		observation.ProviderPaidAt = &paid
	}
	return observation, nil
}

func (g *gateway) ClosePayment(ctx context.Context, ref types.OrderRef) (types.CloseResult, error) {
	if err := g.validateRef(ref); err != nil {
		return types.CloseResult{}, err
	}
	response, err := g.client.TradeClose(ctx, sdk.TradeClose{OutTradeNo: ref.TradeNo, TradeNo: ref.ProviderTransactionID})
	if err != nil {
		return types.CloseResult{State: types.ObservationUnknown}, err
	}
	if response == nil {
		return types.CloseResult{State: types.ObservationUnknown}, errors.New("支付宝关单响应为空")
	}
	if !response.IsSuccess() {
		return types.CloseResult{State: types.ObservationUnknown, ErrorCode: response.SubCode}, fmt.Errorf("支付宝关单未确认: %s", response.Code)
	}
	if response.OutTradeNo != ref.TradeNo || (ref.ProviderTransactionID != "" && response.TradeNo != ref.ProviderTransactionID) {
		return types.CloseResult{State: types.ObservationUnknown}, errors.New("支付宝关单交易引用不符")
	}
	return types.CloseResult{State: types.ObservationClosed}, nil
}
