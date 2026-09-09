package payment

import (
	"context"
	"errors"
	"one-api/common/config"
	"one-api/common/logger"
	"one-api/model"
	"one-api/payment/types"
)

func validateBusiness(p *model.Payment) error {
	if p.Name == "" || p.FixedFee < 0 || p.PercentFee < 0 {
		return errors.New("支付名称或费率无效")
	}
	_, err := types.CurrencyExponent(string(p.Currency))
	return err
}
func CreateGateway(ctx context.Context, p *model.Payment) error {
	if err := validateBusiness(p); err != nil {
		return err
	}
	factory, err := GetFactory(p.Type)
	if err != nil {
		return err
	}
	binding, err := factory.ValidateConfig(ctx, types.GatewayConfigInput{Config: p.Config, Currency: string(p.Currency), Product: p.DefaultProduct})
	if err != nil {
		return err
	}
	p.ID = 0
	p.Identity = binding.Identity
	p.TransactionNamespace = binding.TransactionNamespace
	p.Config = binding.Config
	p.DefaultProduct = binding.DefaultProduct
	p.DefaultMethod = binding.DefaultMethod
	p.CredentialRevision = 1
	p.SetupStatus = "configuring"
	p.SetupError = ""
	if err := p.Insert(); err != nil {
		return err
	}

	client, cancel, err := newCandidate(p.Snapshot())
	if err != nil {
		setSetupFailure(ctx, p.ID, p.CredentialRevision, "client_init_failed")
		return err
	}
	published := false
	defer func() {
		if !published {
			retireCandidate(client, cancel)
		}
	}()
	if _, err := validateCapabilities(client, p.DefaultProduct, string(p.Currency)); err != nil {
		setSetupFailure(ctx, p.ID, p.CredentialRevision, "invalid_capabilities")
		return err
	}
	if configurer, ok := client.(CallbackConfigurer); ok {
		operationCtx, stop := context.WithTimeout(ctx, OperationTimeout)
		result, setupErr := configurer.ConfigureCallbacks(operationCtx, types.CallbackEndpoint{URL: (&PaymentService{Payment: p}).getNotifyURL(configurationServerAddress())})
		stop()
		if setupErr != nil || !result.Ready {
			setSetupFailure(ctx, p.ID, p.CredentialRevision, "callback_setup_failed")
			if setupErr != nil {
				return setupErr
			}
			return errors.New("支付通知配置尚未就绪")
		}
		if result.Config != "" && result.Config != p.Config {
			p.Config = result.Config
			retireCandidate(client, cancel)
			client, cancel, err = newCandidate(p.Snapshot())
			if err != nil {
				setSetupFailure(ctx, p.ID, p.CredentialRevision, "client_init_failed")
				return err
			}
		}
	}
	result := model.DB.WithContext(ctx).Model(&model.Payment{}).Where("id = ? AND credential_revision = ?", p.ID, p.CredentialRevision).Updates(map[string]any{"setup_status": "ready", "setup_error": "", "config": p.Config})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return errors.New("支付配置版本冲突")
	}
	p.SetupStatus = "ready"
	// 发布接管候选的退役责任，包括发布失败；旧句柄仅完成其在途操作。
	published = true
	if err := Resources.Publish(p.Snapshot(), client, cancel); err != nil {
		setSetupFailure(ctx, p.ID, p.CredentialRevision, "client_publish_failed")
		return err
	}
	return nil
}
func setSetupFailure(ctx context.Context, id int, revision int64, code string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), OperationTimeout)
	defer cancel()
	result := model.DB.WithContext(ctx).Unscoped().Model(&model.Payment{}).Where("id = ? AND credential_revision = ?", id, revision).Updates(map[string]any{"setup_status": "failed", "setup_error": code})
	if result.Error != nil {
		logger.SysError("记录支付配置失败状态失败: " + result.Error.Error())
	}
}
func UpdateGateway(ctx context.Context, input *model.Payment) error {
	current, err := model.GetPaymentByID(input.ID)
	if err != nil {
		return err
	}
	if err := validateBusiness(input); err != nil {
		return err
	}
	if input.DefaultProduct == "" {
		input.DefaultProduct = current.DefaultProduct
	}

	enabling := input.Enable != nil && *input.Enable && (current.Enable == nil || !*current.Enable)
	if enabling && current.SetupStatus != "ready" {
		return errors.New("支付配置尚未就绪")
	}
	if enabling || input.DefaultProduct != current.DefaultProduct || input.Currency != current.Currency {
		handle, err := Resources.Acquire(ctx, current.Snapshot())
		if err != nil {
			return err
		}
		defer handle.Release()
		if _, err := validateCapabilities(handle.Client, input.DefaultProduct, string(input.Currency)); err != nil {
			return err
		}
	}
	return input.Update(true)
}
func RotateCredentials(ctx context.Context, id int, revision int64, configJSON string) (*model.Payment, error) {
	current, err := model.GetHistoricalPaymentByID(id)
	if err != nil {
		return nil, err
	}
	if current.CredentialRevision != revision {
		return nil, errors.New("凭证版本已变化，请重新读取")
	}
	factory, err := GetFactory(current.Type)
	if err != nil {
		return nil, err
	}
	binding, err := factory.ValidateConfig(ctx, types.GatewayConfigInput{Config: configJSON, Currency: string(current.Currency), Product: current.DefaultProduct})
	if err != nil {
		return nil, err
	}
	if !binding.Identity.Equal(current.Identity) || binding.TransactionNamespace != current.TransactionNamespace {
		return nil, errors.New("凭证轮换必须保持原商户、应用、环境和协议身份")
	}
	candidate := *current
	candidate.Config = binding.Config
	candidate.CredentialRevision++
	client, cancel, err := newCandidate(candidate.Snapshot())
	if err != nil {
		return nil, err
	}
	published := false
	defer func() {
		if !published {
			retireCandidate(client, cancel)
		}
	}()
	if _, err := validateCapabilities(client, candidate.DefaultProduct, string(candidate.Currency)); err != nil {
		return nil, err
	}
	if configurer, ok := client.(CallbackConfigurer); ok {
		operationCtx, stop := context.WithTimeout(ctx, OperationTimeout)
		result, setupErr := configurer.ConfigureCallbacks(operationCtx, types.CallbackEndpoint{URL: (&PaymentService{Payment: &candidate}).getNotifyURL(configurationServerAddress())})
		stop()
		if setupErr != nil {
			return nil, setupErr
		}
		if !result.Ready {
			return nil, errors.New("新凭证的通知配置未就绪")
		}
		if result.Config != "" && result.Config != candidate.Config {
			candidate.Config = result.Config
			retireCandidate(client, cancel)
			client, cancel, err = newCandidate(candidate.Snapshot())
			if err != nil {
				return nil, err
			}
		}
	}
	if err := model.RotatePaymentCredentials(ctx, id, revision, candidate.Config); err != nil {
		return nil, err
	}
	if err := Resources.Publish(candidate.Snapshot(), client, cancel); err != nil {
		published = true
		setSetupFailure(ctx, id, candidate.CredentialRevision, "client_publish_failed")
		return nil, err
	}
	published = true
	candidate.SetupStatus = "ready"
	return &candidate, nil
}

func configurationServerAddress() string {
	return config.GlobalOption.RuntimeSnapshot().String("ServerAddress", config.ServerAddress)
}

func newCandidate(snapshot types.GatewaySnapshot) (GatewayClient, context.CancelFunc, error) {
	ctx, cancel := context.WithCancel(Resources.ctx)
	client, err := newBoundClient(ctx, snapshot)
	if err != nil {
		retireCandidate(client, cancel)
		return nil, nil, err
	}
	return client, cancel, nil
}
func retireCandidate(client GatewayClient, cancel context.CancelFunc) {
	if cancel != nil {
		cancel()
	}
	if closer, ok := client.(GatewayResourceCloser); ok {
		if err := closer.CloseResources(); err != nil {
			logger.SysError("支付候选资源退役失败: " + err.Error())
		}
	}
}
