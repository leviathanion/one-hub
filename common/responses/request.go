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
	ResponsesCreate      Operation = "responses.create.http"
	ResponsesCompact     Operation = "responses.compact.http"
	ResponsesInputTokens Operation = "responses.input_tokens.http"
)

type RawEnvelope struct {
	Object          *jsonobject.Object
	Projection      types.OpenAIResponsesRequest
	ProjectionError error
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
	RawQuery  string
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
	projection, projectionErr, err := projectRawRequest(object.Raw)
	if err != nil {
		return nil, fmt.Errorf("decode responses envelope: %w", err)
	}
	return &RawEnvelope{
		Object:          object,
		Projection:      projection,
		ProjectionError: projectionErr,
	}, nil
}

func ProjectRawRequest(raw []byte) (types.OpenAIResponsesRequest, error) {
	projection, _, err := projectRawRequest(raw)
	return projection, err
}

func projectRawRequest(raw []byte) (types.OpenAIResponsesRequest, error, error) {
	var projection types.OpenAIResponsesRequest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	projectionErr := decoder.Decode(&projection)
	if projectionErr != nil {
		var err error
		projection, err = decodeCoreProjection(raw)
		if err != nil {
			return types.OpenAIResponsesRequest{}, nil, err
		}
		projectionErr = fmt.Errorf("decode responses projection: %w", projectionErr)
	}
	return projection, projectionErr, nil
}

// decodeCoreProjection reads only fields whose meaning belongs to the proxy:
// transport selection, routing/ownership, bounded admission and billing
// context. Provider-owned unions remain in Object and are validated by the
// selected upstream unless a cross-protocol adapter must interpret them.
func decodeCoreProjection(raw []byte) (types.OpenAIResponsesRequest, error) {
	type coreProjection struct {
		Input                any    `json:"input,omitempty"`
		Model                string `json:"model"`
		Background           *bool  `json:"background,omitempty"`
		Conversation         any    `json:"conversation,omitempty"`
		MaxOutputTokens      int    `json:"max_output_tokens,omitempty"`
		PreviousResponseID   string `json:"previous_response_id,omitempty"`
		Prompt               any    `json:"prompt,omitempty"`
		PromptCacheKey       string `json:"prompt_cache_key,omitempty"`
		PromptCacheRetention string `json:"prompt_cache_retention,omitempty"`
		SafetyIdentifier     string `json:"safety_identifier,omitempty"`
		ServiceTier          string `json:"service_tier,omitempty"`
		ProcessingClass      string `json:"processing_class,omitempty"`
		Store                *bool  `json:"store,omitempty"`
		Stream               bool   `json:"stream,omitempty"`
	}
	var core coreProjection
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&core); err != nil {
		return types.OpenAIResponsesRequest{}, err
	}
	return types.OpenAIResponsesRequest{
		Input:                core.Input,
		Model:                core.Model,
		Background:           core.Background,
		Conversation:         core.Conversation,
		MaxOutputTokens:      core.MaxOutputTokens,
		PreviousResponseID:   core.PreviousResponseID,
		Prompt:               core.Prompt,
		PromptCacheKey:       core.PromptCacheKey,
		PromptCacheRetention: core.PromptCacheRetention,
		SafetyIdentifier:     core.SafetyIdentifier,
		ServiceTier:          core.ServiceTier,
		ProcessingClass:      core.ProcessingClass,
		Store:                core.Store,
		Stream:               core.Stream,
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
