package azure

import (
	"encoding/json"

	"one-api/model"
	"one-api/providers/base"
	"one-api/providers/openai"
	"one-api/types"
)

func (AzureProviderFactory) AssessChatRequest(channel *model.Channel, canonicalModel string, request *types.ChatCompletionRequest) (base.ChatRequestSupport, error) {
	if err := base.RequireOperationEndpoint(getAzureConfig().ChatCompletions, "Chat Completions"); err != nil {
		return base.ChatRequestSupport{}, err
	}
	return base.ChatRequestSupport{}, nil
}

func (AzureProviderFactory) AssessChatWireRequest(channel *model.Channel, canonicalModel string, request *types.ChatCompletionRequest, fields map[string]json.RawMessage) error {
	return openai.ValidateChatRequestForChannel(channel, canonicalModel, request, fields)
}

func (AzureProviderFactory) AssessSpeechRequest(_ *model.Channel, _ *types.SpeechAudioRequest) error {
	return base.RequireOperationEndpoint(getAzureConfig().AudioSpeech, "Speech")
}

func (AzureProviderFactory) AssessTranscriptionRequest(_ *model.Channel, request *types.AudioRequest) error {
	if err := base.RequireOperationEndpoint(getAzureConfig().AudioTranscriptions, "Transcription"); err != nil {
		return err
	}
	return openai.ValidateTranscriptionBillingEvidence(request)
}

func (AzureProviderFactory) ResponsesSupport(_ *model.Channel) base.OperationSupport {
	return azureResponsesSupport(getAzureConfig())
}

func (AzureProviderFactory) ValidateResponsesRequest(channel *model.Channel, operation base.Operation, fields map[string]json.RawMessage, modelName string, applyPreAdd bool) error {
	return openai.ValidateResponsesRequestForChannel(channel, operation, fields, modelName, applyPreAdd)
}

func azureResponsesSupport(config base.ProviderConfig) base.OperationSupport {
	support := base.OperationSupport{Operations: map[base.Operation]base.DataPath{}}
	if config.Responses != "" {
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
