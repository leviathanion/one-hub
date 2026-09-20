package relay

import (
	"net/http"
	"one-api/common"
	"one-api/common/config"
	providersBase "one-api/providers/base"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

type relayImageEdits struct {
	relayBase
	request types.ImageEditRequest
}

func NewRelayImageEdits(c *gin.Context) *relayImageEdits {
	relay := &relayImageEdits{}
	relay.c = c
	return relay
}

func (r *relayImageEdits) setRequest() error {
	if err := common.UnmarshalBodyReusable(r.c, &r.request); err != nil {
		return err
	}
	if err := prepareImageRequest(r.c); err != nil {
		return err
	}

	if r.request.Model == "" {
		r.request.Model = "dall-e-2"
	}

	if r.request.Size == "" {
		r.request.Size = "1024x1024"
	}

	r.setOriginalModel(r.request.Model)
	setRequestChannelCapability(r.c, requireImageEndpoint(r.c, config.RelayModeImagesEdits))

	return nil
}

func (r *relayImageEdits) IsStream() bool { return imageStreamIntent(r.c) }

func (r *relayImageEdits) getPromptTokens() (int, error) {
	return common.CountTokenImage(r.request)
}

func (r *relayImageEdits) send() (err *types.OpenAIErrorWithStatusCode, done bool) {
	r.request.Model = r.modelName
	if provider, ok := r.provider.(providersBase.ImageEditsResponseInterface); ok && provider.SupportsImageResponse() {
		response, apiErr := provider.CreateImageEditsResponse(&r.request)
		if apiErr != nil {
			return apiErr, apiErr.UpstreamAccepted || apiErr.UpstreamAmbiguous
		}
		return r.responseImageClient(response), true
	}
	provider, ok := r.provider.(providersBase.ImageEditsInterface)
	if !ok {
		err = common.StringErrorWrapperLocal("channel not implemented", "channel_error", http.StatusServiceUnavailable)
		done = true
		return
	}

	r.request.Model = r.modelName

	response, err := provider.CreateImageEdits(&r.request)
	if err != nil {
		return
	}
	err = responseJsonClient(r.c, response)

	if err != nil {
		done = true
	}

	return
}
