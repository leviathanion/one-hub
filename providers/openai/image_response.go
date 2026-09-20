package openai

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"

	"one-api/common"
	"one-api/common/config"
	"one-api/common/providerresponse"
	"one-api/common/requester"
	"one-api/types"
)

const imageResponseMaxBytes = 64 << 20

func (p *OpenAIProvider) SupportsImageResponse() bool { return p.usesNativeOpenAIWire() }

func (p *OpenAIProvider) CreateImageGenerationsResponse(request *types.ImageRequest) (*types.ImageResponseWrapper, *types.OpenAIErrorWithStatusCode) {
	req, apiErr := p.GetRequestTextBody(config.RelayModeImagesGenerations, request.Model, request)
	if apiErr != nil {
		return nil, apiErr
	}
	defer req.Body.Close()
	return p.sendImageResponse(req)
}

func (p *OpenAIProvider) CreateImageEditsResponse(request *types.ImageEditRequest) (*types.ImageResponseWrapper, *types.OpenAIErrorWithStatusCode) {
	req, apiErr := p.getRequestImageBody(config.RelayModeImagesEdits, request.Model, request)
	if apiErr != nil {
		return nil, apiErr
	}
	defer req.Body.Close()
	return p.sendImageResponse(req)
}

// One send, followed by media-type dispatch. Client stream intent cannot make
// a JSON upstream response into SSE, or cause the work to be submitted twice.
func (p *OpenAIProvider) sendImageResponse(req *http.Request) (*types.ImageResponseWrapper, *types.OpenAIErrorWithStatusCode) {
	body, err := requestBodyCopy(req)
	if err != nil {
		return nil, common.ErrorWrapperLocal(err, "read_request_body_failed", http.StatusInternalServerError)
	}
	// 只观察实际发送的 stream 意图来选择传输时限，参数语义仍由上游判断。
	// 非流式生成可能长时间不返回响应头，应保留普通请求的配置时限。
	profile := requester.HTTPProfileWorkAction
	if ImageStreamRequested(body, req.Header.Get("Content-Type")) {
		profile = requester.HTTPProfileLongStream
	}
	httpRequester := p.Requester.ForHTTPProfile(profile)
	response, apiErr := httpRequester.SendRequestRawCheckedNativeDialect(req, providerresponse.OperationUnknown)
	if apiErr != nil {
		return nil, apiErr
	}
	if response == nil || response.Body == nil {
		return nil, imageResponseError(fmt.Errorf("provider image response is missing"))
	}
	mediaType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if strings.EqualFold(mediaType, "text/event-stream") {
		observer := &imageStreamEvidence{target: p.Usage}
		p.captureProviderResponseHeaders(response)
		return &types.ImageResponseWrapper{Stream: response, ObserveProviderEvent: observer.Observe}, nil
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, imageResponseMaxBytes+1))
	if err != nil {
		return nil, imageResponseError(err)
	}
	if len(raw) > imageResponseMaxBytes {
		return nil, imageResponseError(fmt.Errorf("provider image response exceeds byte limit"))
	}
	result := &OpenAIProviderImageResponse{}
	if err := result.DecodeCapturedProviderJSON(raw); err != nil {
		return nil, imageResponseError(err)
	}
	result.SetProviderRawJSON(raw)
	result.EnableProviderRawJSONReplay()
	if ErrorHandle(&result.OpenAIErrorResponse) == nil {
		applyImageEvidence(p.Usage, &result.ImageResponse, result.providerDataCount)
	}
	p.captureProviderResponseHeaders(response, true)
	return &types.ImageResponseWrapper{JSON: &result.ImageResponse}, nil
}

func imageResponseError(err error) *types.OpenAIErrorWithStatusCode {
	result := common.ErrorWrapper(err, "invalid_provider_response", http.StatusBadGateway)
	result.UpstreamAccepted = true
	return result
}

// This observer owns only evidence, never stream progress. Repeated completion
// snapshots do not multiply operations or token usage. Inconsistent snapshots
// invalidate the affected billing component while delivery continues.
type imageStreamEvidence struct {
	target        *types.Usage
	usage         []byte
	image         [32]byte
	imageSeen     bool
	imageConflict bool
	tokenConflict bool
}

func (o *imageStreamEvidence) Observe(payload []byte) {
	if o == nil || o.target == nil {
		return
	}
	var event struct {
		Type    string          `json:"type"`
		Model   string          `json:"model"`
		B64JSON string          `json:"b64_json"`
		Usage   json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(payload, &event) != nil {
		return
	}
	if event.Type != "image_generation.completed" && event.Type != "image_edit.completed" {
		return
	}
	var response types.ImageResponse
	response.Model = event.Model
	if len(event.Usage) > 0 && !bytes.Equal(bytes.TrimSpace(event.Usage), []byte("null")) {
		var usage types.ResponsesUsage
		if json.Unmarshal(event.Usage, &usage) == nil {
			// The images streaming protocol defines output_tokens as image tokens.
			// This protocol mapping is evidence, not an estimate or a wire rewrite.
			if usage.OutputTokensDetails == nil && usage.ProviderTokenFields["output_tokens"] {
				usage.OutputTokensDetails = &types.ResponsesUsageOutputTokensDetails{ImageTokens: usage.OutputTokens}
				usage.ProviderTokenFields[config.UsageExtraOutputTextTokens] = true
				usage.ProviderTokenFields[config.UsageExtraOutputImageTokens] = true
			}
			canonical, _ := json.Marshal(usage)
			if len(o.usage) > 0 && !bytes.Equal(canonical, o.usage) {
				o.tokenConflict = true
			}
			o.usage = canonical
			response.Usage = &usage
		}
	}
	previousModel, previousTier, previousConflict := o.target.ResponseModel, o.target.ServiceTier, o.target.AttributionConflict
	applyImageEvidence(o.target, &response, nil)
	o.target.MergeProviderAttribution(previousModel, previousTier)
	o.target.AttributionConflict = o.target.AttributionConflict || previousConflict
	o.target.ProviderTokenConflict = o.target.ProviderTokenConflict || o.tokenConflict
	if event.B64JSON != "" {
		image := sha256.Sum256([]byte(event.B64JSON))
		if o.imageSeen && image != o.image {
			o.imageConflict = true
		}
		o.image, o.imageSeen = image, true

	}
	if o.imageConflict {
		o.target.ProviderOperationUnits = nil
	} else if o.imageSeen {
		o.target.MarkProviderOperationUnits(1)
	}
}
