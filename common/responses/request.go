package responses

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"one-api/common/jsonobject"
	"one-api/common/requestctx"
	"one-api/types"
)

type Operation string

const (
	ResponsesCreate  Operation = "responses.create.http"
	ResponsesCompact Operation = "responses.compact.http"
)

type RawEnvelope struct {
	Object     *jsonobject.Object
	Projection types.OpenAIResponsesRequest
}

type DownstreamDialect string

const (
	DownstreamResponses       DownstreamDialect = "responses"
	DownstreamChatCompletions DownstreamDialect = "chat_completions"
)

type RequestPurpose string

const (
	RequestPurposeChannelProbe RequestPurpose = "channel_probe"
)

type requestPurposeContextKey struct{}

func ContextWithRequestPurpose(ctx context.Context, purpose RequestPurpose) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if purpose == "" {
		return ctx
	}
	return context.WithValue(ctx, requestPurposeContextKey{}, purpose)
}

func RequestPurposeFromContext(ctx context.Context) RequestPurpose {
	if ctx == nil {
		return ""
	}
	purpose, _ := ctx.Value(requestPurposeContextKey{}).(RequestPurpose)
	return purpose
}

type Control struct {
	DownstreamDialect DownstreamDialect
	Stream            bool
	Purpose           RequestPurpose
}

type PromptCacheSource string

const (
	PromptCacheClientBody    PromptCacheSource = "client_body"
	PromptCacheRouteHint     PromptCacheSource = "route_hint"
	PromptCacheChannelPolicy PromptCacheSource = "channel_policy"
)

type PromptCacheDecision struct {
	Key    string
	Source PromptCacheSource
}

type PolicyInput struct {
	PromptCache *PromptCacheDecision
}

type Request struct {
	Operation Operation
	Headers   requestctx.HeaderSnapshot
	Body      *RawEnvelope
	Control   Control
	Policy    PolicyInput
	Principal requestctx.Principal
	ChannelID int
	Model     string
}

func ParseRawEnvelope(raw []byte) (*RawEnvelope, error) {
	object, err := jsonobject.Parse(raw)
	if err != nil {
		return nil, err
	}
	var projection types.OpenAIResponsesRequest
	decoder := json.NewDecoder(bytes.NewReader(object.Raw))
	decoder.UseNumber()
	if err := decoder.Decode(&projection); err != nil {
		return nil, fmt.Errorf("decode responses projection: %w", err)
	}
	return &RawEnvelope{
		Object:     object,
		Projection: projection,
	}, nil
}

func ProjectRequest(req *Request, normalizeModel func(string) string) *types.OpenAIResponsesRequest {
	if req == nil || req.Body == nil {
		return &types.OpenAIResponsesRequest{}
	}
	normalize := func(model string) string {
		model = strings.TrimSpace(model)
		if normalizeModel != nil {
			return normalizeModel(model)
		}
		return model
	}

	request := req.Body.Projection
	if model := normalize(req.Model); model != "" {
		request.Model = model
	} else {
		request.Model = normalize(request.Model)
	}
	request.Stream = req.Control.Stream
	request.ConvertChat = req.Control.DownstreamDialect == DownstreamChatCompletions
	if strings.TrimSpace(request.PromptCacheKey) == "" && req.Policy.PromptCache != nil {
		request.PromptCacheKey = strings.TrimSpace(req.Policy.PromptCache.Key)
	}
	return &request
}
