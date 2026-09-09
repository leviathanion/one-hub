package gemini

import (
	"bytes"
	"encoding/json"
	"net/http"
	"one-api/common"
	"one-api/common/providerresponse"
	"one-api/common/requestctx"
	"one-api/common/requester"
	"one-api/types"
	"strings"
)

type GeminiRelayStreamHandler struct {
	Usage                *types.Usage
	Prefix               string
	ModelName            string
	RequireCachedContent bool
	RequireInputImage    bool
	expectedCandidates   int
	observedCandidates   map[int64]struct{}
	finishedCandidates   map[int64]struct{}
	terminalObserved     bool

	key                string
	framer             *requester.SSEEventFramer
	ErrorSeen          bool
	ProviderCredential string
}

func (p *GeminiProvider) CreateGeminiChat(request *GeminiChatRequest) (*GeminiChatResponse, *types.OpenAIErrorWithStatusCode) {
	raw, _ := p.GetRawBody()
	if request.UsesGoogleSearchInRaw(raw) {
		return nil, common.StringErrorWrapperLocal("Google Search grounding has no configured provider-unit price contract", "gemini_grounding_billing_unsupported", http.StatusBadRequest)
	}
	req, errWithCode := p.getChatRequest(request, true)
	if errWithCode != nil {
		return nil, errWithCode
	}
	defer req.Body.Close()

	geminiResponse := &GeminiChatResponse{}
	geminiResponse.EnableProviderRawJSONCapture()
	// 发送请求
	httpResponse, errWithCode := p.Requester.SendRequestPreservingNativeDialect(req, geminiResponse, false)
	if errWithCode != nil {
		return nil, errWithCode
	}
	if p.Context != nil && httpResponse != nil {
		p.Context.Set(requestctx.ProviderResponseHeadersContextKey, requestctx.SafeProviderResponseHeaders(httpResponse.Header))
		p.Context.Set(requestctx.ProviderResponseStatusContextKey, httpResponse.StatusCode)
	}

	usage := p.GetUsage()
	if geminiResponse.UsageMetadata != nil {
		*usage = ConvertOpenAIUsage(geminiResponse.UsageMetadata, firstNonEmpty(geminiResponse.ModelVersion, geminiResponse.Model))
		applyGeminiUsageRequirements(usage, request.UsesCachedContent(), request.UsesInputImages())
	}
	geminiResponse.EnableProviderRawJSONReplay()

	return geminiResponse, nil
}

func (p *GeminiProvider) CreateGeminiChatStream(request *GeminiChatRequest) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode) {
	raw, _ := p.GetRawBody()
	if request.UsesGoogleSearchInRaw(raw) {
		return nil, common.StringErrorWrapperLocal("Google Search grounding has no configured provider-unit price contract", "gemini_grounding_billing_unsupported", http.StatusBadRequest)
	}
	req, errWithCode := p.getChatRequest(request, true)
	if errWithCode != nil {
		return nil, errWithCode
	}
	defer req.Body.Close()

	channel := p.GetChannel()
	rawRequest, _ := p.GetRawBody()

	chatHandler := &GeminiRelayStreamHandler{
		Usage:                p.Usage,
		ModelName:            request.Model,
		Prefix:               `data: `,
		RequireCachedContent: request.UsesCachedContent(),
		RequireInputImage:    request.UsesInputImages(),
		expectedCandidates:   geminiExpectedCandidateCount(request, rawRequest),

		key:                channel.Key,
		framer:             requester.NewSSEEventFramer(16 << 20),
		ProviderCredential: channel.Key,
	}

	// 发送请求
	streamRequester := p.Requester.ForHTTPProfile(requester.HTTPProfileLongStream)
	resp, errWithCode := streamRequester.SendRequestRawCheckedNativeDialect(req, providerresponse.OperationUnknown)
	if errWithCode != nil {
		return nil, errWithCode
	}

	if p.Context != nil && resp != nil {
		p.Context.Set(requestctx.ProviderResponseHeadersContextKey, requestctx.SafeProviderResponseHeaders(resp.Header))
		p.Context.Set(requestctx.ProviderResponseStatusContextKey, resp.StatusCode)
	}
	stream, errWithCode := requester.RequestNoTrimStreamWithEmitterOptions(streamRequester, resp, chatHandler.HandlerStreamWithEmitter, requester.StreamReadOptions{
		RequireProtocolTerminal:   true,
		ProtocolTerminalPredicate: chatHandler.protocolTerminalObserved,
	})
	if errWithCode != nil {
		return nil, errWithCode
	}

	return stream, nil
}

func (h *GeminiRelayStreamHandler) HandlerStreamWithEmitter(rawLine *[]byte, emitter requester.StreamEmitter[string]) {
	if h == nil || rawLine == nil {
		return
	}
	if h.framer == nil {
		h.framer = requester.NewSSEEventFramer(16 << 20)
	}
	event, complete, err := h.framer.PushLine(*rawLine)
	*rawLine = nil
	if err != nil {
		*rawLine = requester.StreamClosed
		emitter.SendError(err)
		return
	}
	if !complete {
		return
	}
	payload := geminiSSEPayload(event)
	var geminiResponse GeminiChatResponse
	if json.Unmarshal(payload, &geminiResponse) != nil {
		safe, _ := common.RedactCredentialValuesText(string(event), h.ProviderCredential)
		emitter.SendData(safe)
		return
	}
	if geminiResponse.ErrorInfo != nil {
		cleaningError(geminiResponse.ErrorInfo, h.key)
	}
	if geminiResponse.UsageMetadata != nil {
		usage := ConvertOpenAIUsage(geminiResponse.UsageMetadata, firstNonEmpty(geminiResponse.ModelVersion, geminiResponse.Model))
		usage.MergeProviderAttribution(h.Usage.ResponseModel, h.Usage.ServiceTier)
		applyGeminiUsageRequirements(&usage, h.RequireCachedContent, h.RequireInputImage)
		*h.Usage = usage
	}
	safeEvent, _ := common.RedactCredentialValuesText(string(event), h.ProviderCredential)
	if !emitter.SendData(safeEvent) {
		return
	}
	if geminiResponse.ErrorInfo != nil {
		h.ErrorSeen = true
		*rawLine = requester.StreamClosed
		emitter.SendError(geminiResponse.ErrorInfo)
		return
	}
	// Keep reading after a valid whole-response fact. The predicate supplied to
	// the reader accepts only the eventual physical EOF, so trailing usage and
	// provider extensions remain observable and transport timeouts stay errors.
	h.geminiResponseIsTerminal(&geminiResponse)
}

// geminiExpectedCandidateCount is the request-side completion contract. The
// provider may send one response event per candidate, so a candidate's
// finishReason cannot establish a whole-response terminal fact for a request
// that asked for more candidates.
func geminiExpectedCandidateCount(request *GeminiChatRequest, rawRequest []byte) int {
	if request != nil && request.GenerationConfig.CandidateCount > 0 {
		return request.GenerationConfig.CandidateCount
	}
	if len(rawRequest) > 0 {
		// 原生请求保持 raw wire；终态观察也须识别 ProtoJSON 的两种字段名。
		// 这里只提取候选数，不改写请求或替上游校验其他参数。
		var raw map[string]json.RawMessage
		if json.Unmarshal(rawRequest, &raw) == nil {
			for _, configField := range []string{"generationConfig", "generation_config"} {
				var generationConfig map[string]json.RawMessage
				if json.Unmarshal(raw[configField], &generationConfig) != nil {
					continue
				}
				for _, countField := range []string{"candidateCount", "candidate_count"} {
					var count int
					if json.Unmarshal(generationConfig[countField], &count) == nil && count > 0 {
						return count
					}
				}
			}
		}
	}
	return 1
}

// geminiResponseIsTerminal records a whole-response terminal fact separately
// from a candidate's finishReason. A prompt block is an explicit whole
// response outcome; otherwise every observed/requested candidate must finish
// before physical EOF is accepted.
func (h *GeminiRelayStreamHandler) geminiResponseIsTerminal(response *GeminiChatResponse) bool {
	if h == nil || response == nil {
		return false
	}
	if response.PromptFeedback != nil && strings.TrimSpace(response.PromptFeedback.BlockReason) != "" {
		h.terminalObserved = true
		return true
	}

	if h.observedCandidates == nil {
		h.observedCandidates = make(map[int64]struct{})
	}
	if h.finishedCandidates == nil {
		h.finishedCandidates = make(map[int64]struct{})
	}
	for _, candidate := range response.Candidates {
		h.observedCandidates[candidate.Index] = struct{}{}
		if candidate.FinishReason != nil && strings.TrimSpace(*candidate.FinishReason) != "" {
			h.finishedCandidates[candidate.Index] = struct{}{}
		} else {
			delete(h.finishedCandidates, candidate.Index)
		}
	}
	expected := h.expectedCandidates
	if expected <= 0 {
		expected = 1
	}
	if len(h.observedCandidates) == expected && len(h.finishedCandidates) == expected {
		h.terminalObserved = true
	} else if len(response.Candidates) > 0 {
		h.terminalObserved = false
	}
	return h.terminalObserved
}

func (h *GeminiRelayStreamHandler) protocolTerminalObserved() bool {
	if h == nil || !h.terminalObserved {
		return false
	}
	return h.framer == nil || h.framer.BufferedLen() == 0
}

func geminiSSEPayload(event []byte) []byte {
	data := make([]string, 0, 1)
	for _, line := range strings.Split(string(event), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	return []byte(strings.Join(data, "\n"))
}

func (h *GeminiRelayStreamHandler) HandlerStream(rawLine *[]byte, dataChan chan string, errChan chan error) {
	rawStr := string(*rawLine)
	// 如果rawLine 前缀不为data:，则直接返回
	if !strings.HasPrefix(rawStr, h.Prefix) {
		dataChan <- rawStr
		return
	}

	noSpaceLine := bytes.TrimSpace(*rawLine)
	noSpaceLine = noSpaceLine[6:]

	var geminiResponse GeminiChatResponse
	err := json.Unmarshal(noSpaceLine, &geminiResponse)
	if err != nil {
		errChan <- ErrorToGeminiErr(err)
		return
	}

	if geminiResponse.ErrorInfo != nil {
		cleaningError(geminiResponse.ErrorInfo, h.key)
		errChan <- geminiResponse.ErrorInfo
		return
	}
	if geminiResponse.UsageMetadata == nil {
		dataChan <- rawStr
		return
	}

	usage := ConvertOpenAIUsage(geminiResponse.UsageMetadata, firstNonEmpty(geminiResponse.ModelVersion, geminiResponse.Model))

	usage.MergeProviderAttribution(h.Usage.ResponseModel, h.Usage.ServiceTier)
	applyGeminiUsageRequirements(&usage, h.RequireCachedContent, h.RequireInputImage)
	*h.Usage = usage

	dataChan <- rawStr
}
