package azure_v1

import (
	"encoding/json"

	"one-api/model"
	"one-api/providers/base"
	"one-api/providers/openai"
	"one-api/types"
)

func (AzureV1ProviderFactory) AssessChatRequest(channel *model.Channel, canonicalModel string, request *types.ChatCompletionRequest) (base.ChatRequestSupport, error) {
	if err := base.RequireOperationEndpoint(getAzureConfig().ChatCompletions, "Chat Completions"); err != nil {
		return base.ChatRequestSupport{}, err
	}
	return base.ChatRequestSupport{}, nil
}

func (AzureV1ProviderFactory) AssessChatWireRequest(channel *model.Channel, canonicalModel string, request *types.ChatCompletionRequest, fields map[string]json.RawMessage) error {
	return openai.ValidateChatRequestForChannel(channel, canonicalModel, request, fields)
}

func (AzureV1ProviderFactory) AssessSpeechRequest(_ *model.Channel, _ *types.SpeechAudioRequest) error {
	return base.RequireOperationEndpoint(getAzureConfig().AudioSpeech, "Speech")
}

func (AzureV1ProviderFactory) AssessTranscriptionRequest(_ *model.Channel, request *types.AudioRequest) error {
	if err := base.RequireOperationEndpoint(getAzureConfig().AudioTranscriptions, "Transcription"); err != nil {
		return err
	}
	return openai.ValidateTranscriptionBillingEvidence(request)
}

func (AzureV1ProviderFactory) ResponsesSupport(_ *model.Channel) base.OperationSupport {
	support := base.OperationSupport{Operations: map[base.Operation]base.DataPath{}}
	if getAzureConfig().Responses != "" {
		for _, operation := range []base.Operation{
			base.OperationResponsesCreate,
			base.OperationResponsesCompact,
			base.OperationResponsesInputTokens,
			base.OperationResponsesRetrieve,
			base.OperationResponsesDelete,
			base.OperationResponsesInputItems,
			base.OperationResponsesWebSocket,
		} {
			support.Operations[operation] = base.DataPathSameDialect
		}
	}
	return support
}

func (AzureV1ProviderFactory) ValidateResponsesRequest(channel *model.Channel, operation base.Operation, fields map[string]json.RawMessage, modelName string, applyPreAdd bool) error {
	return openai.ValidateResponsesRequestForChannel(channel, operation, fields, modelName, applyPreAdd)
}
