package minimax

import (
	"encoding/json"
	"strings"

	"one-api/model"
	"one-api/providers/base"
	"one-api/providers/openai"
	"one-api/types"
)

func (MiniMaxProviderFactory) AssessChatRequest(channel *model.Channel, canonicalModel string, request *types.ChatCompletionRequest) (base.ChatRequestSupport, error) {
	if err := base.RequireOperationEndpoint(getConfig().ChatCompletions, "Chat Completions"); err != nil {
		return base.ChatRequestSupport{}, err
	}
	return base.ChatRequestSupport{}, nil
}

func (MiniMaxProviderFactory) AssessChatWireRequest(channel *model.Channel, canonicalModel string, request *types.ChatCompletionRequest, fields map[string]json.RawMessage) error {
	return openai.ValidateChatRequestForChannel(channel, canonicalModel, request, fields)
}

func (MiniMaxProviderFactory) AssessSpeechRequest(_ *model.Channel, request *types.SpeechAudioRequest) error {
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
