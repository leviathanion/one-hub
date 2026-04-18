package relay

import (
	"one-api/common"
	"one-api/common/surface"
	"one-api/model"
	"one-api/relay/relay_util"
	"one-api/types"
	"time"

	providersBase "one-api/providers/base"

	"github.com/gin-gonic/gin"
)

type relayBase struct {
	c              *gin.Context
	provider       providersBase.ProviderInterface
	originalModel  string
	modelName      string
	contract       surface.Contract
	allowHeartbeat bool
	heartbeat      *relay_util.Heartbeat

	firstResponseTime time.Time
}

type RelayBaseInterface interface {
	send() (err *types.OpenAIErrorWithStatusCode, done bool)
	getPromptTokens() (int, error)
	setRequest() error
	getRequest() any
	setProvider(modelName string) error
	getProvider() providersBase.ProviderInterface
	getOriginalModel() string
	getModelName() string
	getContext() *gin.Context
	IsStream() bool
	// HandleError(err *types.OpenAIErrorWithStatusCode)
	GetFirstResponseTime() time.Time

	HandleJsonError(err *types.OpenAIErrorWithStatusCode)
	HandleStreamError(err *types.OpenAIErrorWithStatusCode)
	SetHeartbeat(isStream bool) *relay_util.Heartbeat
}

type relaySetupErrorWrapper interface {
	WrapSetupError(stage string, err error) *types.OpenAIErrorWithStatusCode
}

func (r *relayBase) getRequest() interface{} {
	return nil
}

func (r *relayBase) IsStream() bool {
	return false
}

func (r *relayBase) setProvider(modelName string) error {
	common.SetRequestBodyReparseNeeded(r.c, false)

	if provider, newModelName, ok := consumeCachedProviderSelection(r.c, modelName); ok {
		r.provider = provider
		r.modelName = newModelName
		return nil
	}

	provider, modelName, fail := GetProvider(r.c, modelName)
	if fail != nil {
		return fail
	}
	r.provider = provider
	r.modelName = modelName
	_, _ = applyPreMappingForProvider(r.c, modelName, provider)

	return nil
}

func (r *relayBase) getContract() surface.Contract {
	if r.contract != nil {
		return r.contract
	}
	return surface.OpenAIContract()
}

func (r *relayBase) setOriginalModel(modelName string) {
	r.originalModel = modelName
}

func (r *relayBase) getContext() *gin.Context {
	return r.c
}

func (r *relayBase) getProvider() providersBase.ProviderInterface {
	return r.provider
}

func (r *relayBase) getOriginalModel() string {
	return r.originalModel
}

func (r *relayBase) getModelName() string {
	billingOriginalModel := r.c.GetBool("billing_original_model")

	if billingOriginalModel {
		return r.originalModel
	}
	return r.modelName
}

func (r *relayBase) GetFirstResponseTime() time.Time {
	return r.firstResponseTime
}

func (r *relayBase) SetFirstResponseTime(firstResponseTime time.Time) {
	r.firstResponseTime = firstResponseTime
}

func (r *relayBase) GetError(err *types.OpenAIErrorWithStatusCode) (int, any) {
	newErr := surface.NormalizeOpenAIError(r.c, err)
	return newErr.StatusCode, types.OpenAIErrorResponse{
		Error: newErr.OpenAIError,
	}
}

func (r *relayBase) HandleJsonError(err *types.OpenAIErrorWithStatusCode) {
	surfaceErr := surface.FromOpenAIError(err)
	surface.LogLocalError(r.c, surfaceErr)
	r.getContract().RenderJSONError(r.c, surfaceErr)
}

func (r *relayBase) HandleStreamError(err *types.OpenAIErrorWithStatusCode) {
	surfaceErr := surface.FromOpenAIError(err)
	surface.LogLocalError(r.c, surfaceErr)
	r.getContract().RenderStreamError(r.c, surfaceErr)
}

func (r *relayBase) SetHeartbeat(isStream bool) *relay_util.Heartbeat {
	if !r.allowHeartbeat {
		return nil
	}

	setting, exists := r.c.Get("token_setting")
	if !exists {
		return nil
	}

	tokenSetting, ok := setting.(*model.TokenSetting)
	if !ok || !tokenSetting.Heartbeat.Enabled {
		return nil
	}

	r.heartbeat = relay_util.NewHeartbeat(
		isStream,
		relay_util.HeartbeatConfig{
			TimeoutSeconds:  tokenSetting.Heartbeat.TimeoutSeconds,
			IntervalSeconds: 5, // 5s 发送一次心跳
		},
		r.c,
	)
	r.heartbeat.Start()

	return r.heartbeat
}
