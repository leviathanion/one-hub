package relay

import (
	"net/http"
	"one-api/common"
	"one-api/common/providerresponse"
	"one-api/model"
	"one-api/providers"
	providersBase "one-api/providers/base"
	"one-api/types"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

type relaySpeech struct {
	relayBase
	request types.SpeechAudioRequest
}

func NewRelaySpeech(c *gin.Context) *relaySpeech {
	relay := &relaySpeech{}
	relay.c = c
	return relay
}

func (r *relaySpeech) setRequest() error {
	if err := common.UnmarshalBodyReusable(r.c, &r.request); err != nil {
		return err
	}

	r.setOriginalModel(r.request.Model)
	setRequestChannelCapability(r.c, requireSpeechChannelCompatibility(r.request.Model, &r.request))
	return nil
}

func (r *relaySpeech) getPromptTokens() (int, error) {
	return len(r.request.Input), nil
}

func (r *relaySpeech) IsStream() bool {
	return strings.EqualFold(strings.TrimSpace(r.request.StreamFormat), "sse")
}

func requireSpeechChannelCompatibility(modelName string, request *types.SpeechAudioRequest) requestChannelCapability {
	return func(channel *model.Channel) error {
		canonicalModel, err := mappedModelForChannel(channel, modelName)
		if err != nil {
			return &capabilityGateError{message: "channel has invalid model mapping configuration", status: http.StatusServiceUnavailable}
		}
		effective := *request
		effective.Model = canonicalModel
		return providerCapabilityGateError(providers.AssessSpeechRequest(channel, &effective))
	}
}

func (r *relaySpeech) send() (err *types.OpenAIErrorWithStatusCode, done bool) {
	provider, ok := r.provider.(providersBase.SpeechInterface)
	if !ok {
		err = common.StringErrorWrapperLocal("channel not implemented", "channel_error", http.StatusServiceUnavailable)
		done = true
		return
	}

	r.request.Model = r.modelName

	response, err := provider.CreateSpeech(&r.request)
	if err != nil {
		return
	}
	if r.IsStream() {
		if r.heartbeat != nil {
			r.heartbeat.Stop()
		}
		var firstResponse time.Time
		var observeProviderEvent func([]byte)
		if observer, ok := provider.(interface{ ObserveSpeechEvent([]byte) }); ok {
			observeProviderEvent = observer.ObserveSpeechEvent
		}
		firstResponse, err = responseAudioSSEClient(r.c, response, audioSSESpeech, providerresponse.OperationBinaryDownload, observeProviderEvent)
		r.SetFirstResponseTime(firstResponse)
		return err, err != nil
	}
	err = responseMultipart(r.c, response, providerresponse.Policy{
		Operation:      providerresponse.OperationBinaryDownload,
		DataPath:       providerresponse.DataPathSameDialect,
		BodyUnmodified: true,
	})

	if err != nil {
		done = true
	}

	return
}
