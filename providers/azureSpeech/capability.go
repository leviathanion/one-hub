package azureSpeech

import (
	"strings"

	"one-api/model"
	"one-api/providers/base"
	"one-api/types"
)

func (AzureSpeechProviderFactory) AssessSpeechRequest(_ *model.Channel, request *types.SpeechAudioRequest) error {
	if request == nil {
		return &base.RequestCapabilityError{Param: "request", Message: "speech request is required"}
	}
	if _, ok := request.VoiceString(); !ok {
		return &base.RequestCapabilityError{Param: "voice", Message: "selected speech adapter requires voice to be a string"}
	}
	if strings.EqualFold(strings.TrimSpace(request.StreamFormat), "sse") {
		return &base.RequestCapabilityError{Param: "stream_format", Message: "selected speech adapter does not support SSE audio streaming"}
	}
	return nil
}
