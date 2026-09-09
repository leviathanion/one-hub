package openai

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"strings"

	"one-api/model"
	"one-api/providers/base"
	"one-api/types"
)

const imageStreamFieldMaxBytes int64 = 64

// ValidateImageStreamRequestBody checks the final image request body without
// interpreting any provider-owned fields. Image adapters in this package only
// have a unary response path, so an explicit stream=true cannot be sent.
func ValidateImageStreamRequestBody(body []byte, contentType string) error {
	if imageStreamRequested(body, contentType) {
		return &base.RequestCapabilityError{Param: "stream", Message: "image streaming is not supported"}
	}
	return nil
}

func imageStreamRequested(body []byte, contentType string) bool {
	if mediaType, params, err := mime.ParseMediaType(contentType); err == nil && strings.HasPrefix(strings.ToLower(mediaType), "multipart/") {
		boundary := strings.TrimSpace(params["boundary"])
		if boundary == "" {
			return false
		}
		reader := multipart.NewReader(bytes.NewReader(body), boundary)
		for {
			part, err := reader.NextRawPart()
			if err != nil {
				return false
			}
			if part.FormName() != "stream" {
				continue
			}
			value, err := io.ReadAll(io.LimitReader(part, imageStreamFieldMaxBytes+1))
			_ = part.Close()
			if err != nil || int64(len(value)) > imageStreamFieldMaxBytes {
				return false
			}
			return strings.TrimSpace(string(value)) == "true"
		}
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return false
	}
	raw, ok := fields["stream"]
	if !ok {
		return false
	}
	var requested bool
	return json.Unmarshal(raw, &requested) == nil && requested
}

func (OpenAIProviderFactory) AssessChatRequest(channel *model.Channel, canonicalModel string, request *types.ChatCompletionRequest) (base.ChatRequestSupport, error) {
	return AssessChatRequestForChannel(channel, canonicalModel, request, "https://api.openai.com")
}

func (OpenAIProviderFactory) AssessChatWireRequest(channel *model.Channel, canonicalModel string, request *types.ChatCompletionRequest, fields map[string]json.RawMessage) error {
	return ValidateChatRequestForChannel(channel, canonicalModel, request, fields)
}

func (OpenAIProviderFactory) AssessSpeechRequest(channel *model.Channel, request *types.SpeechAudioRequest) error {
	return AssessSpeechRequestForChannel(channel, request, "https://api.openai.com")
}

func (OpenAIProviderFactory) AssessTranscriptionRequest(channel *model.Channel, request *types.AudioRequest) error {
	return AssessTranscriptionRequestForChannel(channel, request, "https://api.openai.com")
}

func (OpenAIProviderFactory) ResponsesSupport(channel *model.Channel) base.OperationSupport {
	return ResponsesSupportForChannel(channel, "https://api.openai.com")
}

func (OpenAIProviderFactory) ValidateResponsesRequest(channel *model.Channel, operation base.Operation, fields map[string]json.RawMessage, modelName string, applyPreAdd bool) error {
	return ValidateResponsesRequestForChannel(channel, operation, fields, modelName, applyPreAdd)
}

func AssessChatRequestForChannel(channel *model.Channel, canonicalModel string, request *types.ChatCompletionRequest, defaultBaseURL string) (base.ChatRequestSupport, error) {
	if model.IsOpenAIDataResidencyChannel(channel) {
		return base.ChatRequestSupport{}, &base.RequestCapabilityError{Param: "operation", Message: "OpenAI data-residency endpoints are not supported"}
	}
	if err := base.RequireOperationEndpoint(getOpenAIConfig(defaultBaseURL, channel).ChatCompletions, "Chat Completions"); err != nil {
		return base.ChatRequestSupport{}, err
	}
	return base.ChatRequestSupport{}, nil
}

// ValidateChatRequestForChannel evaluates only proxy-owned billing and
// OnlyChat facts from the OpenAI-dialect post-conversion map. It deliberately
// leaves unrelated future unions opaque. pre_add is already present in request.
func ValidateChatRequestForChannel(channel *model.Channel, modelName string, request *types.ChatCompletionRequest, fields map[string]json.RawMessage) error {
	if request == nil {
		return nil
	}
	requestMap := make(map[string]any)
	if fields != nil {
		for _, name := range []string{"store", "web_search_options", "tools"} {
			raw, present := fields[name]
			if !present {
				continue
			}
			var value any
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.UseNumber()
			if err := decoder.Decode(&value); err != nil {
				return &base.RequestCapabilityError{Param: name, Message: "Chat request cannot be evaluated for billing evidence"}
			}
			requestMap[name] = value
		}
	} else {
		body, err := json.Marshal(request)
		if err != nil || json.Unmarshal(body, &requestMap) != nil {
			return &base.RequestCapabilityError{Param: "request", Message: "Chat request cannot be evaluated for billing evidence"}
		}
	}
	return validateChatRequestMapForChannel(channel, modelName, requestMap)
}

func validateChatRequestMapForChannel(channel *model.Channel, modelName string, requestMap map[string]any) error {
	if channel != nil {
		customParams, err := channel.GetCustomParameterMap()
		if err != nil {
			return &base.RequestCapabilityError{Param: "custom_parameter", Message: "channel custom parameters are invalid"}
		}
		requestMap = base.ApplyCustomParams(requestMap, customParams, modelName, false)
	}
	requestMap["model"] = modelName
	if value, present := requestMap["store"]; present && value != nil {
		if store, valid := value.(bool); !valid || store {
			return &base.RequestCapabilityError{Param: "store", Message: "store must be false or null because Stored Chat lifecycle is unavailable"}
		}
	}
	if webSearch, present := requestMap["web_search_options"]; present && webSearch != nil {
		return &base.RequestCapabilityError{Param: "web_search_options", Message: "Chat Search does not expose provider execution-count evidence; use Responses web search"}
	}
	modelLower := strings.ToLower(strings.TrimSpace(modelName))
	if strings.Contains(modelLower, "search-preview") || strings.HasPrefix(modelLower, "gpt-5-search-api") {
		return &base.RequestCapabilityError{Param: "model", Message: "Chat Search does not expose provider execution-count evidence; use Responses web search"}
	}
	if channel != nil && channel.OnlyChat {
		if tools, present := requestMap["tools"]; present && customParameterToolsPresent(tools) {
			return &base.RequestCapabilityError{Param: "tools", Message: "channel only supports requests without tools"}
		}
	}
	return nil
}

func (p *OpenAIProvider) validateChatRequestPolicy(request *types.ChatCompletionRequest) error {
	requestMap, exists, err := p.GetRawBodyMap()
	if err != nil {
		return &base.RequestCapabilityError{Param: "request", Message: "Chat request cannot be evaluated for billing evidence"}
	}
	if exists {
		return validateChatRequestMapForChannel(p.Channel, request.Model, requestMap)
	}
	return ValidateChatRequestForChannel(p.Channel, request.Model, request, nil)
}

func customParameterToolsPresent(value any) bool {
	if value == nil {
		return false
	}
	if tools, ok := value.([]any); ok {
		return len(tools) > 0
	}
	return true
}

func AssessSpeechRequestForChannel(channel *model.Channel, request *types.SpeechAudioRequest, defaultBaseURL string) error {
	if request == nil {
		return &base.RequestCapabilityError{Param: "request", Message: "speech request is required"}
	}
	return base.RequireOperationEndpoint(getOpenAIConfig(defaultBaseURL, channel).AudioSpeech, "Speech")
}

func AssessTranscriptionRequestForChannel(channel *model.Channel, request *types.AudioRequest, defaultBaseURL string) error {
	if request == nil {
		return &base.RequestCapabilityError{Param: "request", Message: "transcription request is required"}
	}
	if err := base.RequireOperationEndpoint(getOpenAIConfig(defaultBaseURL, channel).AudioTranscriptions, "Transcription"); err != nil {
		return err
	}
	return ValidateTranscriptionBillingEvidence(request)
}

func ValidateTranscriptionBillingEvidence(request *types.AudioRequest) error {
	if request == nil {
		return &base.RequestCapabilityError{Param: "request", Message: "transcription request is required"}
	}
	if !request.Stream && !hasJSONTranscriptionResponse(request) {
		return &base.RequestCapabilityError{Param: "response_format", Message: "the selected transcription response format cannot carry provider billing usage"}
	}
	return nil
}

func ResponsesSupportForChannel(channel *model.Channel, defaultBaseURL string) base.OperationSupport {
	support := base.OperationSupport{Operations: map[base.Operation]base.DataPath{}}
	if channel == nil || model.IsOpenAIDataResidencyChannel(channel) || getOpenAIConfig(defaultBaseURL, channel).Responses == "" {
		return support
	}
	path := base.DataPathSameDialect
	if isOfficialOpenAIWireChannel(channel, defaultBaseURL) {
		path = base.DataPathExactWire
		for _, operation := range []base.Operation{
			base.OperationResponsesCreate,
			base.OperationResponsesCompact,
			base.OperationResponsesInputTokens,
			base.OperationResponsesRetrieve,
			base.OperationResponsesDelete,
			base.OperationResponsesInputItems,
			base.OperationResponsesWebSocket,
		} {
			support.Operations[operation] = path
		}
		return support
	}
	for _, operation := range []base.Operation{base.OperationResponsesCreate, base.OperationResponsesCompact, base.OperationResponsesInputTokens} {
		support.Operations[operation] = path
	}
	stored, _ := channel.GetOtherBoolField("responses_stored_lifecycle")
	if stored {
		for _, operation := range []base.Operation{base.OperationResponsesRetrieve, base.OperationResponsesDelete, base.OperationResponsesInputItems} {
			support.Operations[operation] = path
		}
	}
	websocket, _ := channel.GetOtherBoolField("responses_ws_native")
	if websocket {
		support.Operations[base.OperationResponsesWebSocket] = path
	}
	return support
}

func ValidateResponsesRequestForChannel(channel *model.Channel, operation base.Operation, fields map[string]json.RawMessage, modelName string, applyPreAdd bool) error {
	return ValidateResponsesCustomParameterCompatibility(channel, fields, modelName, operation, applyPreAdd)
}
