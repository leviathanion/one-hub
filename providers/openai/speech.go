package openai

import (
	"encoding/json"
	"math"
	"net/http"
	"one-api/common/config"
	"one-api/common/providerresponse"
	"one-api/common/requester"
	"one-api/types"
	"strings"
)

func (p *OpenAIProvider) CreateSpeech(request *types.SpeechAudioRequest) (*http.Response, *types.OpenAIErrorWithStatusCode) {
	req, errWithCode := p.GetRequestTextBody(config.RelayModeAudioSpeech, request.Model, request)
	if errWithCode != nil {
		return nil, errWithCode
	}
	defer req.Body.Close()

	// 发送请求
	streamRequester := p.Requester.ForHTTPProfile(requester.HTTPProfileLongStream)
	var resp *http.Response
	if p.ProviderRawJSONReplay {
		resp, errWithCode = streamRequester.SendRequestRawCheckedPreservingRedirect(req, providerresponse.OperationBinaryDownload)
	} else {
		resp, errWithCode = streamRequester.SendRequestRaw(req)
	}
	if errWithCode != nil {
		return nil, errWithCode
	}
	p.captureProviderResponseHeaders(resp)

	if resp.Header.Get("Content-Type") == "application/json" {
		return nil, requester.HandleErrorResp(resp, streamRequester.ErrorHandler, streamRequester.PrefixProviderErrors, streamRequester.ReplayOpenAIErrorEnvelopes)
	}

	p.Usage.TotalTokens = p.Usage.PromptTokens

	return resp, nil
}

// ObserveSpeechEvent accepts only a complete Speech terminal snapshot.  The
// relay invokes this callback for each decoded SSE payload; deltas and empty
// terminal payloads therefore leave any previously trusted usage untouched.
func (p *OpenAIProvider) ObserveSpeechEvent(payload []byte) {
	if p == nil || p.Usage == nil {
		return
	}

	var event struct {
		Type        string            `json:"type"`
		Usage       *types.AudioUsage `json:"usage"`
		Model       string            `json:"model,omitempty"`
		ServiceTier string            `json:"service_tier,omitempty"`
	}
	if json.Unmarshal(payload, &event) != nil || providerresponse.SSEPayloadContainsError("", payload) || !isSpeechUsageTerminal(event.Type) {
		return
	}
	applyOpenAISpeechUsage(p.Usage, event.Usage, event.Model, event.ServiceTier)
}

func isSpeechUsageTerminal(eventType string) bool {
	switch strings.ToLower(strings.TrimSpace(eventType)) {
	case "speech.audio.done", "audio.done":
		return true
	default:
		return false
	}
}

func applyOpenAISpeechUsage(target *types.Usage, providerUsage *types.AudioUsage, actualModel, serviceTier string) {
	if target == nil || providerUsage == nil || !validOpenAISpeechTokenUsage(providerUsage) {
		return
	}

	inputTokens := *providerUsage.InputTokens
	outputTokens := *providerUsage.OutputTokens
	totalTokens := *providerUsage.TotalTokens
	candidate := types.Usage{
		PromptTokens:     inputTokens,
		CompletionTokens: outputTokens,
		TotalTokens:      totalTokens,
		ProviderTokenFields: map[string]bool{
			"prompt_tokens":     true,
			"completion_tokens": true,
			"total_tokens":      true,
		},
	}
	if providerUsage.InputDetails != nil {
		if providerUsage.InputDetails.TextTokens != nil {
			candidate.PromptTokensDetails.TextTokens = *providerUsage.InputDetails.TextTokens
			candidate.MarkProviderTokenField(config.UsageExtraInputTextTokens)
		}
		if providerUsage.InputDetails.AudioTokens != nil {
			candidate.PromptTokensDetails.AudioTokens = *providerUsage.InputDetails.AudioTokens
			candidate.MarkProviderTokenField(config.UsageExtraInputAudio)
		}
	}
	candidate.MergeProviderAttribution(actualModel, serviceTier)
	candidate.MarkProviderReported()

	previousModel := target.ResponseModel
	previousTier := target.ServiceTier
	previousConflict := target.AttributionConflict
	*target = candidate
	target.MergeProviderAttribution(previousModel, previousTier)
	target.AttributionConflict = target.AttributionConflict || previousConflict
}

func validOpenAISpeechTokenUsage(providerUsage *types.AudioUsage) bool {
	if providerUsage == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(providerUsage.Type)) {
	case "", "tokens":
	default:
		return false
	}
	if providerUsage.InputTokens == nil || providerUsage.OutputTokens == nil || providerUsage.TotalTokens == nil {
		return false
	}
	inputTokens := int64(*providerUsage.InputTokens)
	outputTokens := int64(*providerUsage.OutputTokens)
	totalTokens := int64(*providerUsage.TotalTokens)
	if inputTokens < 0 || outputTokens < 0 || totalTokens < 0 || inputTokens > math.MaxInt64-outputTokens || inputTokens+outputTokens != totalTokens {
		return false
	}
	if providerUsage.InputDetails != nil {
		for _, detail := range []*int{providerUsage.InputDetails.TextTokens, providerUsage.InputDetails.AudioTokens} {
			if detail != nil && *detail < 0 {
				return false
			}
		}
	}
	return true
}
