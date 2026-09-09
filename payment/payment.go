package payment

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"one-api/payment/gateway/alipay"
	"one-api/payment/gateway/epay"
	"one-api/payment/gateway/stripe"
	"one-api/payment/gateway/wxpay"
	"one-api/payment/types"
)

type GatewayFactory = types.GatewayFactory
type GatewayClient = types.GatewayClient
type CallbackReceiver = types.CallbackReceiver
type PaymentQuerier = types.PaymentQuerier
type PaymentCloser = types.PaymentCloser
type CallbackConfigurer = types.CallbackConfigurer
type GatewayResourceCloser = types.GatewayResourceCloser

var registry = struct {
	sync.RWMutex
	factories map[string]GatewayFactory
}{factories: map[string]GatewayFactory{}}

func RegisterFactory(factory GatewayFactory) {
	registry.Lock()
	defer registry.Unlock()
	registry.factories[factory.Descriptor().Kind] = factory
}
func GetFactory(kind string) (GatewayFactory, error) {
	registry.RLock()
	defer registry.RUnlock()
	factory := registry.factories[kind]
	if factory == nil {
		return nil, errors.New("不支持的支付网关")
	}
	return factory, nil
}
func init() {
	RegisterFactory(&epay.Factory{})
	RegisterFactory(&alipay.Factory{})
	RegisterFactory(&wxpay.Factory{})
	RegisterFactory(&stripe.Factory{})
}

func validateCapabilities(client GatewayClient, product, currency string) (types.Capabilities, error) {
	c, err := client.Capabilities(product)
	if err != nil {
		return c, err
	}
	if !slices.Contains(c.Currencies, currency) {
		return c, errors.New("支付产品不支持此币种")
	}
	if c.PreparationMode != types.BrowserHandoff && c.PreparationMode != types.ServerCreate {
		return c, errors.New("支付产品准备模式无效")
	}
	if c.DisplayWindow <= 0 {
		return c, errors.New("支付产品必须提供有限本地付款窗口")
	}
	_, callback := client.(CallbackReceiver)
	_, query := client.(PaymentQuerier)
	_, closePayment := client.(PaymentCloser)
	_, setup := client.(CallbackConfigurer)
	if c.CanReceiveCallbacks != callback || (c.QueryByMerchantRef || c.QueryByResourceRef) && !query || c.CanClose != closePayment || c.CanConfigureCallbacks != setup {
		return c, errors.New("支付能力与适配器接口不一致")
	}
	if !callback && !(query && (c.QueryByMerchantRef || c.QueryByResourceRef)) {
		return c, errors.New("支付产品没有可信的付款确认路径")
	}
	for _, kind := range c.ActionKinds {
		if !slices.Contains([]string{"redirect", "form", "qr_code"}, kind) {
			return c, fmt.Errorf("前端不支持支付动作 %s", kind)
		}
	}
	return c, nil
}

func newBoundClient(ctx context.Context, snapshot types.GatewaySnapshot) (GatewayClient, error) {
	factory, err := GetFactory(snapshot.Identity.Kind)
	if err != nil {
		return nil, err
	}
	return factory.NewClient(ctx, snapshot)
}
