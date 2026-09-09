package siliconflow

import (
	"encoding/json"
	"strings"

	"one-api/model"
	"one-api/providers/base"
	"one-api/providers/openai"
	"one-api/types"
)

func (SiliconflowProviderFactory) AssessChatRemoteMedia(_ *model.Channel, _ *types.ChatCompletionRequest, _ base.ChatRemoteMediaSummary) (base.RemoteMediaMode, error) {
	if err := base.RequireOperationEndpoint(getConfig().ChatCompletions, "Chat Completions"); err != nil {
		return base.RemoteMediaReject, err
	}
	return base.RemoteMediaPassURL, nil
}

func (SiliconflowProviderFactory) AssessChatRequest(channel *model.Channel, canonicalModel string, request *types.ChatCompletionRequest) (base.ChatRequestSupport, error) {
	if err := base.RequireOperationEndpoint(getConfig().ChatCompletions, "Chat Completions"); err != nil {
		return base.ChatRequestSupport{}, err
	}
	return base.ChatRequestSupport{}, nil
}

func (SiliconflowProviderFactory) AssessChatWireRequest(channel *model.Channel, canonicalModel string, request *types.ChatCompletionRequest, fields map[string]json.RawMessage) error {
	return openai.ValidateChatRequestForChannel(channel, canonicalModel, request, fields)
}

func (SiliconflowProviderFactory) AssessSpeechRequest(_ *model.Channel, request *types.SpeechAudioRequest) error {
	if err := base.RequireOperationEndpoint(getConfig().AudioSpeech, "Speech"); err != nil {
		return err
	}
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

func (SiliconflowProviderFactory) AssessTranscriptionRequest(_ *model.Channel, request *types.AudioRequest) error {
	if err := base.RequireOperationEndpoint(getConfig().AudioTranscriptions, "Transcription"); err != nil {
		return err
	}
	if err := openai.ValidateTranscriptionBillingEvidence(request); err != nil {
		return err
	}
	if request != nil && request.Stream {
		return &base.RequestCapabilityError{Param: "stream", Message: "selected transcription adapter does not support streaming"}
	}
	return nil
}
