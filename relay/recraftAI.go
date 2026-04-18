package relay

import (
	"errors"
	"net/http"
	"one-api/common"
	"one-api/common/config"
	"one-api/common/surface"
	providersBase "one-api/providers/base"
	"one-api/types"
	"strings"

	"github.com/gin-gonic/gin"
)

var allowRecraftChannelType = []int{config.ChannelTypeRecraft}
var getRecraftRawProviderFunc = getRecraftRawProvider

func RelayRecraftAI(c *gin.Context) {
	Relay(c)
}

func Path2RecraftAIModel(path string) string {
	parts := strings.Split(path, "/")
	lastPart := parts[len(parts)-1]

	return "recraft_" + lastPart
}

func IsRecraftNativePath(path string) bool {
	switch path {
	case "/recraftAI/v1/images/vectorize",
		"/recraftAI/v1/images/removeBackground",
		"/recraftAI/v1/images/clarityUpscale",
		"/recraftAI/v1/images/generativeUpscale",
		"/recraftAI/v1/styles":
		return true
	default:
		return false
	}
}

type relayRecraftNative struct {
	relayBase
	requestURL string
}

func NewRelayRecraftNative(c *gin.Context) *relayRecraftNative {
	c.Set("allow_channel_type", allowRecraftChannelType)
	return &relayRecraftNative{
		relayBase: relayBase{
			c:        c,
			contract: surface.RecraftContract(),
		},
	}
}

func (r *relayRecraftNative) setRequest() error {
	if !IsRecraftNativePath(r.c.Request.URL.Path) {
		return errors.New("unsupported recraft endpoint")
	}

	r.requestURL = strings.TrimPrefix(r.c.Request.URL.Path, "/recraftAI")
	r.setOriginalModel(Path2RecraftAIModel(r.c.Request.URL.Path))
	return nil
}

func (r *relayRecraftNative) setProvider(modelName string) error {
	common.SetRequestBodyReparseNeeded(r.c, false)

	if provider, newModelName, ok := consumeCachedProviderSelection(r.c, modelName); ok {
		rawProvider, rawOK := provider.(providersBase.RawRelayInterface)
		if !rawOK {
			return errors.New("provider not found")
		}
		r.provider = rawProvider
		r.modelName = newModelName
		return nil
	}

	provider, newModelName, fail := getRecraftRawProviderFunc(r.c, modelName)
	if fail != nil {
		return fail
	}

	r.provider = provider
	r.modelName = newModelName
	_, _ = applyPreMappingForProvider(r.c, modelName, provider)
	return nil
}

func (r *relayRecraftNative) getPromptTokens() (int, error) {
	return 1, nil
}

func (r *relayRecraftNative) WrapSetupError(stage string, err error) *types.OpenAIErrorWithStatusCode {
	switch stage {
	case "request":
		return common.StringErrorWrapperLocal(err.Error(), "invalid_request", http.StatusBadRequest)
	case "provider":
		return common.StringErrorWrapperLocal(err.Error(), "provider_not_found", http.StatusServiceUnavailable)
	case "reparse":
		return common.StringErrorWrapperLocal(err.Error(), "one_hub_error", http.StatusBadRequest)
	default:
		return nil
	}
}

func (r *relayRecraftNative) send() (err *types.OpenAIErrorWithStatusCode, done bool) {
	rawProvider, ok := r.provider.(providersBase.RawRelayInterface)
	if !ok {
		err = common.StringErrorWrapperLocal("channel not implemented", "channel_error", http.StatusServiceUnavailable)
		done = true
		return
	}

	response, err := rawProvider.CreateRelay(r.requestURL)
	if err != nil {
		return
	}

	err = responseMultipart(r.c, response)
	if err != nil {
		done = true
	}
	return
}

func getRecraftRawProvider(c *gin.Context, model string) (providersBase.RawRelayInterface, string, error) {
	provider, newModelName, fail := GetProvider(c, model)
	if fail != nil {
		return nil, "", fail
	}

	rawProvider, ok := provider.(providersBase.RawRelayInterface)
	if !ok {
		return nil, "", errors.New("provider not found")
	}

	return rawProvider, newModelName, nil
}
