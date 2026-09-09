package cloudflareAI

import (
	"one-api/model"
	"one-api/providers/base"
	"one-api/types"
)

func (CloudflareAIProviderFactory) AssessChatRequest(_ *model.Channel, _ string, _ *types.ChatCompletionRequest) (base.ChatRequestSupport, error) {
	return base.ChatRequestSupport{}, base.RequireOperationEndpoint(getConfig().ChatCompletions, "Chat Completions")
}

func (CloudflareAIProviderFactory) AssessTranscriptionRequest(_ *model.Channel, request *types.AudioRequest) error {
	if err := base.RequireOperationEndpoint(getConfig().AudioTranscriptions, "Transcription"); err != nil {
		return err
	}
	if request == nil {
		return &base.RequestCapabilityError{Param: "request", Message: "transcription request is required"}
	}
	if request.Stream {
		return &base.RequestCapabilityError{Param: "stream", Message: "selected transcription adapter does not support streaming"}
	}
	return nil
}
