package relay

import (
	"mime"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"one-api/common"
	"one-api/common/providerresponse"
	"one-api/model"
	"one-api/providers"
	"one-api/providers/openai"
	"one-api/types"
)

func prepareImageRequest(c *gin.Context) error {
	if c == nil || c.Request == nil {
		return nil
	}
	body, exists := common.GetCanonicalRequestBody(c)
	if !exists {
		return nil
	}
	mediaType, _, _ := mime.ParseMediaType(c.Request.Header.Get("Content-Type"))
	if strings.EqualFold(mediaType, "application/json") {
		return prepareResourceRequest(c, body, "images")
	}
	return nil
}

func imageStreamIntent(c *gin.Context) bool {
	if c == nil || c.Request == nil {
		return false
	}
	body, _ := common.GetCanonicalRequestBody(c)
	return openai.ImageStreamRequested(body, c.Request.Header.Get("Content-Type"))
}

func requireImageEndpoint(c *gin.Context, relayMode int) requestChannelCapability {
	stream := imageStreamIntent(c)
	return func(channel *model.Channel) error {
		if err := requireEndpointEnabled(relayMode)(channel); err != nil {
			return err
		}
		if stream && !providers.SupportsImageStreaming(channel) {
			return newCapabilityGateError("stream", "selected image adapter does not support streaming")
		}
		return nil
	}
}

// Kept for variations, whose adapter has only a unary delivery contract.
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

func (r *relayBase) responseImageClient(response *types.ImageResponseWrapper) *types.OpenAIErrorWithStatusCode {
	if response == nil {
		return common.StringErrorWrapper("provider image response missing", "invalid_provider_response", http.StatusBadGateway)
	}
	if response.Stream != nil {
		defer response.Stream.Body.Close()
		firstResponse, err := responseNativeSSEClient(r.c, response.Stream, providerresponse.OperationUnknown, response.ObserveProviderEvent)
		r.SetFirstResponseTime(firstResponse)
		return err
	}
	return responseJsonClient(r.c, response.JSON)
}
