package relay

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"one-api/common"
	"one-api/common/config"
	providersBase "one-api/providers/base"
	"one-api/providers/openai"
	"one-api/types"
)

type relayImageGenerations struct {
	relayBase
	request types.ImageRequest
}

func NewRelayImageGenerations(c *gin.Context) *relayImageGenerations {
	relay := &relayImageGenerations{}
	relay.c = c
	return relay
}

func (r *relayImageGenerations) setRequest() error {
	if err := common.UnmarshalBodyReusable(r.c, &r.request); err != nil {
		return err
	}
	if err := rejectImageStreamRequest(r.c); err != nil {
		return err
	}

	if r.request.Model == "" {
		r.request.Model = "dall-e-2"
	}

	if r.request.N == 0 {
		r.request.N = 1
	}

	if strings.HasPrefix(r.request.Model, "dall-e") {
		if r.request.Size == "" {
			r.request.Size = "1024x1024"
		}

		if r.request.Quality == "" {
			r.request.Quality = "standard"
		}
	}

	r.setOriginalModel(r.request.Model)
	setRequestChannelCapability(r.c, requireEndpointEnabled(config.RelayModeImagesGenerations))

	return nil
}

func rejectImageStreamRequest(c *gin.Context) error {
	if c == nil || c.Request == nil {
		return nil
	}
	body, exists := common.GetCanonicalRequestBody(c)
	if !exists {
		return nil
	}
	return providerCapabilityGateError(openai.ValidateImageStreamRequestBody(body, c.Request.Header.Get("Content-Type")))
}

func (r *relayImageGenerations) getPromptTokens() (int, error) {
	return common.CountTokenImage(r.request)
}

func (r *relayImageGenerations) send() (err *types.OpenAIErrorWithStatusCode, done bool) {
	provider, ok := r.provider.(providersBase.ImageGenerationsInterface)
	if !ok {
		err = common.StringErrorWrapperLocal("channel not implemented", "channel_error", http.StatusServiceUnavailable)
		done = true
		return
	}

	r.request.Model = r.modelName

	response, err := provider.CreateImageGenerations(&r.request)
	if err != nil {
		return
	}
	err = responseJsonClient(r.c, response)

	if err != nil {
		done = true
	}

	return
}
