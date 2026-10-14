package antigravity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	"one-api/common"
	"one-api/common/providerresponse"
	"one-api/common/requestctx"
	"one-api/common/requester"
	commonresponses "one-api/common/responses"
	"one-api/providers/antigravity/wire"
	"one-api/providers/base"
	"one-api/providers/claude"
	"one-api/providers/gemini"
	"one-api/types"
)

// convertedRequest 在所有凭据和网络操作之前完成转换；factory 使用相同函数。
func convertedRequest(channelParams map[string]interface{}, dialect wire.Dialect, raw []byte, modelName string) (*wire.Request, error) {
	converted, err := wire.ConvertRequest(dialect, raw, modelName)
	if err != nil {
		return nil, err
	}
	converted.Body, err = base.ApplyRawCustomParams(converted.Body, channelParams, modelName, false)
	if err != nil {
		return nil, err
	}
	if err := wire.FinalizeRequest(converted); err != nil {
		return nil, err
	}
	if (&gemini.NativeRequest{Raw: converted.Body}).UsesGoogleSearch() {
		return nil, &wire.CapabilityError{Param: "tools", Message: "Google Search grounding 尚无 provider-unit 计费契约"}
	}
	return converted, nil
}

func (p *AntigravityProvider) convert(dialect wire.Dialect, raw []byte, modelName string, stream bool) (*wire.Request, *types.OpenAIErrorWithStatusCode) {
	params, err := p.CustomParameterHandler()
	if err != nil {
		return nil, requestError(err)
	}
	converted, err := convertedRequest(params, dialect, raw, modelName)
	if err != nil {
		return nil, requestError(err)
	}
	converted.Context.Stream = stream
	return converted, nil
}

func (p *AntigravityProvider) rawRequest(fallback any) ([]byte, error) {
	raw, ok := p.GetRawBody()
	var err error
	if !ok {
		raw, err = json.Marshal(fallback)
	}
	if err != nil {
		return nil, err
	}
	return raw, nil
}

func (p *AntigravityProvider) open(ctx context.Context, request *wire.Request, stream bool) (*http.Response, *types.OpenAIErrorWithStatusCode) {
	if err := p.prepareCredentials(ctx); err != nil {
		result := common.ErrorWrapperLocal(err, "credential_unavailable", http.StatusServiceUnavailable)
		result.UpstreamNotAttempted = true
		return nil, result
	}
	kind, requestID := "agent", "agent-"+uuid.NewString()
	var fields map[string]json.RawMessage
	if json.Unmarshal(request.Body, &fields) == nil {
		generation := fields["generationConfig"]
		if generation == nil {
			generation = fields["generation_config"]
		}
		var settings map[string]json.RawMessage
		_ = json.Unmarshal(generation, &settings)
		modalities := settings["responseModalities"]
		if modalities == nil {
			modalities = settings["response_modalities"]
		}
		var values []string
		_ = json.Unmarshal(modalities, &values)
		for _, modality := range values {
			if modality == "IMAGE" {
				kind, requestID = "image_gen", fmt.Sprintf("image_gen/%d/%s/12", time.Now().UnixMilli(), uuid.NewString())
				break
			}
		}
	}
	envelope, err := json.Marshal(map[string]string{"project": p.credentials.ProjectID, "model": request.Context.Model, "userAgent": "antigravity", "requestType": kind, "requestId": requestID})
	if err != nil {
		return nil, requestError(err)
	}
	// 外层由代理拥有，内层保持 raw；json.Marshal(RawMessage) 会额外压缩内层。
	body := append(envelope[:len(envelope)-1], []byte(`,"request":`)...)
	body = append(body, request.Body...)
	body = append(body, '}')
	action := "generateContent"
	r := p.Requester
	if stream {
		action = "streamGenerateContent?alt=sse"
		r = r.ForHTTPProfile(requester.HTTPProfileLongStream)
	}
	req, err := r.NewRequest(http.MethodPost, p.upstreamURL(action), r.WithContext(ctx), r.WithHeader(p.GetRequestHeaders()), r.WithBody(body))
	if err != nil {
		return nil, requestError(errors.New("无法构造 Antigravity 请求"))
	}
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	var resp *http.Response
	var apiErr *types.OpenAIErrorWithStatusCode
	if request.Context.Dialect == wire.DialectGemini {
		resp, apiErr = r.SendRequestRawCheckedNativeDialect(req, providerresponse.OperationUnknown)
	} else {
		resp, apiErr = r.SendRequestRawCheckedNoRedirect(req)
	}
	if apiErr != nil {
		return nil, providerresponse.SanitizeAPIError(apiErr, p.credentialValues()...)
	}
	if p.Context != nil {
		p.Context.Set(requestctx.ProviderResponseHeadersContextKey, requestctx.SafeProviderResponseHeaders(resp.Header, p.credentialValues()...))
		p.Context.Set(requestctx.ProviderResponseStatusContextKey, resp.StatusCode)
	}
	return resp, nil
}

func (p *AntigravityProvider) usageObserver(request *wire.Request) func(json.RawMessage, string) {
	input := &gemini.NativeRequest{Raw: request.Body}
	return func(raw json.RawMessage, responseModel string) {
		if len(raw) == 0 && responseModel == "" {
			return
		}
		if p.Usage == nil {
			p.Usage = &types.Usage{}
		}
		p.Usage.MergeProviderAttribution(responseModel, "")
		if len(raw) == 0 {
			return
		}
		var reported gemini.GeminiUsageMetadata
		if json.Unmarshal(raw, &reported) != nil {
			// 新的无效快照不能继续借用旧快照的精确 token 资格。
			p.Usage.ProviderTokenConflict = true
			if p.Usage.BillingDiagnostics == nil {
				p.Usage.BillingDiagnostics = make(map[string]bool)
			}
			p.Usage.BillingDiagnostics["invalid_provider_token_usage"] = true
			return
		}
		usage := gemini.ConvertOpenAIUsage(&reported, "")
		usage.MergeProviderAttribution(p.Usage.ResponseModel, p.Usage.ServiceTier)
		usage.AttributionConflict = usage.AttributionConflict || p.Usage.AttributionConflict
		usage.MergeBillingDiagnostics(p.Usage.BillingDiagnostics)
		input.ApplyUsageRequirements(&usage)
		*p.Usage = usage
	}
}

func (p *AntigravityProvider) crossJSON(ctx context.Context, request *wire.Request) ([]byte, *types.OpenAIErrorWithStatusCode) {
	resp, apiErr := p.open(ctx, request, true)
	if apiErr != nil {
		return nil, apiErr
	}
	decoder := wire.NewResponseDecoder(request.Context, "resp_"+uuid.NewString(), false)
	reader := *p.Requester
	reader.Context = ctx
	return newAntigravityAggregate(&reader, resp, decoder, p.usageObserver(request), func(err error) error { return p.responseError(err, false) })
}

func (p *AntigravityProvider) crossStream(ctx context.Context, request *wire.Request) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode) {
	resp, apiErr := p.open(ctx, request, true)
	if apiErr != nil {
		return nil, apiErr
	}
	decoder := wire.NewResponseDecoder(request.Context, "resp_"+uuid.NewString(), true)
	reader := *p.Requester
	reader.Context = ctx
	return newAntigravityStream(&reader, resp, decoder, p.usageObserver(request), func(err error) error { return p.responseError(err, false) })
}

func (p *AntigravityProvider) chatRequest(request *types.ChatCompletionRequest, stream bool) (*wire.Request, *types.OpenAIErrorWithStatusCode) {
	raw, err := p.rawRequest(request)
	if err == nil {
		raw, err = patchChatPreparedMedia(raw, request)
	}
	if err != nil {
		return nil, requestError(err)
	}
	return p.convert(wire.DialectChat, raw, request.Model, stream)
}

func (p *AntigravityProvider) CreateChatCompletion(request *types.ChatCompletionRequest) (*types.ChatCompletionResponse, *types.OpenAIErrorWithStatusCode) {
	converted, apiErr := p.chatRequest(request, false)
	if apiErr != nil {
		return nil, apiErr
	}
	raw, apiErr := p.crossJSON(p.LogContext(), converted)
	if apiErr != nil {
		return nil, apiErr
	}
	result := &types.ChatCompletionResponse{}
	if err := json.Unmarshal(raw, result); err != nil {
		return nil, p.responseError(err, false)
	}
	result.SetProviderRawJSON(raw)
	result.EnableProviderRawJSONReplay()
	return result, nil
}

func (p *AntigravityProvider) CreateChatCompletionStream(request *types.ChatCompletionRequest) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode) {
	converted, apiErr := p.chatRequest(request, true)
	if apiErr != nil {
		return nil, apiErr
	}
	return p.crossStream(p.LogContext(), converted)
}

func (p *AntigravityProvider) claudeRequest(request *claude.ClaudeRequest, stream bool) (*wire.Request, *types.OpenAIErrorWithStatusCode) {
	raw, err := p.rawRequest(request)
	if err == nil {
		raw, err = patchClaudePreparedMedia(raw, request)
	}
	if err != nil {
		return nil, requestError(err)
	}
	return p.convert(wire.DialectClaude, raw, request.Model, stream)
}

func (p *AntigravityProvider) CreateClaudeChat(request *claude.ClaudeRequest) (*claude.ClaudeResponse, *types.OpenAIErrorWithStatusCode) {
	converted, apiErr := p.claudeRequest(request, false)
	if apiErr != nil {
		return nil, apiErr
	}
	raw, apiErr := p.crossJSON(p.LogContext(), converted)
	if apiErr != nil {
		return nil, apiErr
	}
	result := &claude.ClaudeResponse{}
	if err := result.DecodeCapturedProviderJSON(raw); err != nil {
		return nil, p.responseError(err, false)
	}
	result.SetProviderRawJSON(raw)
	result.EnableProviderRawJSONReplay()
	return result, nil
}

func (p *AntigravityProvider) CreateClaudeChatStream(request *claude.ClaudeRequest) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode) {
	converted, apiErr := p.claudeRequest(request, true)
	if apiErr != nil {
		return nil, apiErr
	}
	return p.crossStream(p.LogContext(), converted)
}

func (p *AntigravityProvider) responsesRequest(request *commonresponses.Request, stream bool) (*wire.Request, *types.OpenAIErrorWithStatusCode) {
	if request == nil || request.Operation != commonresponses.ResponsesCreate || request.Body == nil || request.Body.Object == nil {
		return nil, requestError(errors.New("仅支持带 body 的 Responses create"))
	}
	applyPre := request.Control.DownstreamDialect != commonresponses.DownstreamChatCompletions
	if err := base.ValidateResponsesCustomParameterCompatibility(p.Channel, request.Body.Object.Fields, request.Model, base.OperationResponsesCreate, applyPre); err != nil {
		return nil, requestError(err)
	}
	raw, err := request.Body.Object.MarshalJSON()
	if err != nil {
		return nil, requestError(err)
	}
	params, err := p.CustomParameterHandler()
	if err == nil && applyPre {
		raw, err = base.ApplyRawCustomParams(raw, params, request.Model, true)
	}
	if err != nil {
		return nil, requestError(err)
	}
	return p.convert(wire.DialectResponses, raw, request.Model, stream)
}

func (p *AntigravityProvider) CreateResponses(ctx context.Context, request *commonresponses.Request) (*types.OpenAIResponsesResponses, *types.OpenAIErrorWithStatusCode) {
	converted, apiErr := p.responsesRequest(request, false)
	if apiErr != nil {
		return nil, apiErr
	}
	raw, apiErr := p.crossJSON(ctx, converted)
	if apiErr != nil {
		return nil, apiErr
	}
	result := &types.OpenAIResponsesResponses{}
	if err := result.DecodeCapturedProviderJSON(raw); err != nil {
		return nil, p.responseError(err, false)
	}
	result.SetProviderRawJSON(raw)
	result.EnableProviderRawJSONReplay()
	return result, nil
}

func (p *AntigravityProvider) CreateResponsesStream(ctx context.Context, request *commonresponses.Request) (commonresponses.EventStream, *types.OpenAIErrorWithStatusCode) {
	converted, apiErr := p.responsesRequest(request, true)
	if apiErr != nil {
		return nil, apiErr
	}
	stream, apiErr := p.crossStream(ctx, converted)
	if apiErr != nil {
		return nil, apiErr
	}
	return commonresponses.NewEventStream(stream, commonresponses.IgnoreResponsesEvent), nil
}

func (p *AntigravityProvider) CreateGeminiChat(request *gemini.NativeRequest) (*gemini.GeminiChatResponse, *types.OpenAIErrorWithStatusCode) {
	converted, apiErr := p.convert(wire.DialectGemini, request.Raw, request.Model, false)
	if apiErr != nil {
		return nil, apiErr
	}
	resp, apiErr := p.open(p.LogContext(), converted, false)
	if apiErr != nil {
		return nil, apiErr
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (64<<20)+1))
	if err != nil || len(raw) > 64<<20 {
		return nil, p.responseError(errors.New("原生响应读取失败或超过大小限制"), true)
	}
	// 已执行的错误也可能带用量；先观察证据，错误交付仍由解包器决定。
	var envelope map[string]json.RawMessage
	_ = json.Unmarshal(raw, &envelope)
	fields := envelope
	if response := envelope["response"]; len(response) > 0 {
		fields = nil
		_ = json.Unmarshal(response, &fields)
	}
	var responseModel string
	_ = json.Unmarshal(fields["modelVersion"], &responseModel)
	p.usageObserver(converted)(fields["usageMetadata"], responseModel)
	inner, err := wire.UnwrapResponse(raw)
	if err != nil {
		return nil, p.responseError(err, true)
	}
	result := &gemini.GeminiChatResponse{}
	if err := result.DecodeCapturedProviderJSON(inner); err != nil {
		return nil, p.responseError(err, true)
	}
	result.SetProviderRawJSON(inner)
	result.EnableProviderRawJSONReplay()
	return result, nil
}

func (p *AntigravityProvider) CreateGeminiChatStream(request *gemini.NativeRequest) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode) {
	converted, apiErr := p.convert(wire.DialectGemini, request.Raw, request.Model, true)
	if apiErr != nil {
		return nil, apiErr
	}
	resp, apiErr := p.open(p.LogContext(), converted, true)
	if apiErr != nil {
		return nil, apiErr
	}
	reader := *p.Requester
	reader.Context = p.LogContext()
	return newAntigravityNativeStream(&reader, resp, wire.NewNativeDecoder(converted.Context.ExpectedCandidates), p.usageObserver(converted), func(err error) error { return p.responseError(err, true) })
}
