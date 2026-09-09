package relay

import (
	"net/http"
	"one-api/common"
	"one-api/common/providerresponse"
	"one-api/model"
	"one-api/providers"
	providersBase "one-api/providers/base"
	"one-api/types"
	"time"

	"github.com/gin-gonic/gin"
)

type relayTranscriptions struct {
	relayBase
	request types.AudioRequest
}

func NewRelayTranscriptions(c *gin.Context) *relayTranscriptions {
	relay := &relayTranscriptions{}
	relay.c = c
	return relay
}

func (r *relayTranscriptions) setRequest() error {
	if err := common.UnmarshalBodyReusable(r.c, &r.request); err != nil {
		return err
	}

	r.setOriginalModel(r.request.Model)
	setRequestChannelCapability(r.c, requireTranscriptionChannelCompatibility(r.request.Model, &r.request))
	return nil
}

func (r *relayTranscriptions) getPromptTokens() (int, error) {
	return 0, nil
}

func (r *relayTranscriptions) IsStream() bool {
	return r.request.Stream
}

func requireTranscriptionChannelCompatibility(modelName string, request *types.AudioRequest) requestChannelCapability {
	return func(channel *model.Channel) error {
		canonicalModel, err := mappedModelForChannel(channel, modelName)
		if err != nil {
			return &capabilityGateError{message: "channel has invalid model mapping configuration", status: http.StatusServiceUnavailable}
		}
		effective := *request
		effective.Model = canonicalModel
		return providerCapabilityGateError(providers.AssessTranscriptionRequest(channel, &effective))
	}
}

func (r *relayTranscriptions) send() (err *types.OpenAIErrorWithStatusCode, done bool) {
	provider, ok := r.provider.(providersBase.TranscriptionsInterface)
	if !ok {
		err = common.StringErrorWrapperLocal("channel not implemented", "channel_error", http.StatusServiceUnavailable)
		done = true
		return
	}

	r.request.Model = r.modelName

	response, err := provider.CreateTranscriptions(&r.request)
	if err != nil {
		return
	}
	if response == nil {
		return common.StringErrorWrapperLocal("provider returned no transcription response", "invalid_provider_response", http.StatusBadGateway), true
	}
	if response.Stream != nil {
		if r.heartbeat != nil {
			r.heartbeat.Stop()
		}
		var firstResponse time.Time
		firstResponse, err = responseAudioSSEClient(r.c, response.Stream, audioSSETranscription, providerresponse.OperationAudioTranscription, response.ObserveProviderEvent)
		r.SetFirstResponseTime(firstResponse)
		return err, err != nil
	}
	if r.heartbeat != nil {
		r.heartbeat.Stop()
	}
	err = responseCustom(r.c, response, providersBase.OperationAudioTranscription)

	if err != nil {
		done = true
	}

	return
}
