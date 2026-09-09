package relay

import (
	"net/http"
	"one-api/common"
	"one-api/common/surface"
	"one-api/metrics"
	providersBase "one-api/providers/base"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

func RelayRerank(c *gin.Context) {
	relay := NewRelayRerank(c)
	contract := surface.RerankContract()

	if err := relay.setRequest(); err != nil {
		surfaceErr := surface.NewLocalError(http.StatusBadRequest, err.Error(), "invalid_request")
		surface.LogLocalError(c, surfaceErr)
		contract.RenderJSONError(c, surfaceErr)
		return
	}

	if err := relay.setProvider(relay.getOriginalModel()); err != nil {
		surfaceErr := surface.NewLocalError(http.StatusServiceUnavailable, err.Error(), "provider_not_found")
		surface.LogLocalError(c, surfaceErr)
		contract.RenderJSONError(c, surfaceErr)
		return
	}

	apiErr, _ := RelayHandler(relay)
	if apiErr == nil {
		metrics.RecordProvider(c, http.StatusOK)
		return
	}

	channel := relay.getProvider().GetChannel()
	observeRelayProviderFailure(c, channel, apiErr)

	surfaceErr := surface.FromOpenAIError(apiErr)
	surface.LogLocalError(c, surfaceErr)
	contract.RenderJSONError(c, surfaceErr)
}

type relayRerank struct {
	relayBase
	request types.RerankRequest
}

func NewRelayRerank(c *gin.Context) *relayRerank {
	relay := &relayRerank{}
	relay.c = c
	return relay
}

func (r *relayRerank) setRequest() error {
	if err := common.UnmarshalBodyReusable(r.c, &r.request); err != nil {
		return err
	}

	r.setOriginalModel(r.request.Model)

	return nil
}

func (r *relayRerank) getPromptTokens() (int, error) {
	channel := r.provider.GetChannel()
	return common.CountTokenRerankMessages(r.request, r.modelName, channel.PreCost), nil
}

func (r *relayRerank) send() (err *types.OpenAIErrorWithStatusCode, done bool) {
	chatProvider, ok := r.provider.(providersBase.RerankInterface)
	if !ok {
		err = common.StringErrorWrapperLocal("channel not implemented", "channel_error", http.StatusServiceUnavailable)
		done = true
		return
	}

	r.request.Model = r.modelName

	var response *types.RerankResponse
	response, err = chatProvider.CreateRerank(&r.request)
	if err != nil {
		return
	}
	err = responseJsonClient(r.c, response)

	if err != nil {
		done = true
	}

	return
}
