package openai

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"one-api/common"
	"one-api/common/config"
	"one-api/common/requestctx"
	"one-api/common/requester"
	"one-api/types"
	"regexp"
	"strconv"
	"strings"
)

const maxTranscriptionResponseBodyBytes int64 = 16 << 20

func (p *OpenAIProvider) CreateTranscriptions(request *types.AudioRequest) (*types.AudioResponseWrapper, *types.OpenAIErrorWithStatusCode) {
	if err := ValidateTranscriptionBillingEvidence(request); err != nil {
		return nil, common.StringErrorWrapperLocal(err.Error(), "transcription_billing_evidence_unavailable", http.StatusBadRequest)
	}
	req, errWithCode := p.getRequestAudioBody(config.RelayModeAudioTranscription, request.Model, request)
	if errWithCode != nil {
		return nil, errWithCode
	}
	defer req.Body.Close()

	// The request's stream flag describes client intent only. Send exactly once,
	// then choose the delivery parser from the successful provider media type.
	// A long-stream transport profile is still appropriate for stream intent,
	// but it does not decide the response protocol.
	responseRequester := p.Requester
	if request.Stream {
		responseRequester = p.Requester.ForHTTPProfile(requester.HTTPProfileLongStream)
	}
	resp, errWithCode := responseRequester.SendRequest(req, nil, true)
	if errWithCode != nil {
		return nil, errWithCode
	}
	if resp == nil || resp.Body == nil {
		return nil, transcriptionProviderResponseError("provider transcription response is missing")
	}

	mediaType, mediaTypeErr := transcriptionResponseMediaType(resp)
	if mediaTypeErr != nil {
		_ = resp.Body.Close()
		return nil, transcriptionProviderResponseError("provider transcription response has an invalid content type")
	}
	if mediaType == "" {
		// A missing header has no protocol claim. Keep the established request
		// format as a narrow compatibility fallback, while any declared header
		// always wins.
		if hasJSONTranscriptionResponse(request) {
			mediaType = "application/json"
		} else {
			mediaType = "text/plain"
		}
	}

	switch {
	case mediaType == "text/event-stream":
		p.captureProviderResponseHeaders(resp)
		return &types.AudioResponseWrapper{
			Stream: resp,
			ObserveProviderEvent: func(payload []byte) {
				var event struct {
					Usage *types.AudioUsage `json:"usage"`
					Model string            `json:"model,omitempty"`
				}
				if json.Unmarshal(payload, &event) == nil {
					// Duration SSE is observable for its explicit seconds component,
					// but the confirmed per-completion operation fact is limited to
					// JSON and Realtime completion producers.
					applyOpenAITranscriptionUsageForSource(p.Usage, event.Usage, event.Model, false)
				}
			},
		}, nil
	case isJSONMediaType(mediaType):
		return p.readJSONTranscriptionResponse(resp)
	case strings.HasPrefix(mediaType, "text/") && !hasJSONTranscriptionResponse(request):
		return p.readTextTranscriptionResponse(resp)
	default:
		_ = resp.Body.Close()
		return nil, transcriptionProviderResponseError("provider transcription response protocol is unsupported")
	}
}

func transcriptionResponseMediaType(response *http.Response) (string, error) {
	if response == nil {
		return "", nil
	}
	contentType := strings.TrimSpace(response.Header.Get("Content-Type"))
	if contentType == "" {
		return "", nil
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return "", err
	}
	return strings.ToLower(strings.TrimSpace(mediaType)), nil
}

func isJSONMediaType(mediaType string) bool {
	return mediaType == "application/json" || strings.HasSuffix(mediaType, "+json")
}

func (p *OpenAIProvider) readJSONTranscriptionResponse(response *http.Response) (*types.AudioResponseWrapper, *types.OpenAIErrorWithStatusCode) {
	body, err := readTranscriptionResponseBody(response.Body)
	_ = response.Body.Close()
	if err != nil {
		return nil, transcriptionAcceptedResponseError(err, "read_response_body_failed")
	}

	decoded := &OpenAIProviderTranscriptionsResponse{}
	if err := json.Unmarshal(body, decoded); err != nil {
		return nil, transcriptionAcceptedResponseError(err, "decode_response_failed")
	}
	if openaiErr := ErrorHandle(&decoded.OpenAIErrorResponse); openaiErr != nil {
		return nil, &types.OpenAIErrorWithStatusCode{OpenAIError: *openaiErr, StatusCode: http.StatusBadGateway, UpstreamAccepted: true}
	}
	applyOpenAITranscriptionUsage(p.Usage, decoded.Usage, decoded.Model)
	return &types.AudioResponseWrapper{
		Headers: transcriptionResponseHeaders(response.Header),
		Body:    body,
	}, nil
}

func (p *OpenAIProvider) readTextTranscriptionResponse(response *http.Response) (*types.AudioResponseWrapper, *types.OpenAIErrorWithStatusCode) {
	body, err := readTranscriptionResponseBody(response.Body)
	_ = response.Body.Close()
	if err != nil {
		return nil, transcriptionAcceptedResponseError(err, "read_response_body_failed")
	}
	return &types.AudioResponseWrapper{
		Headers: transcriptionResponseHeaders(response.Header),
		Body:    body,
	}, nil
}

func readTranscriptionResponseBody(reader io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, maxTranscriptionResponseBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxTranscriptionResponseBodyBytes {
		return nil, fmt.Errorf("transcription response body exceeds %d bytes", maxTranscriptionResponseBodyBytes)
	}
	return body, nil
}

func transcriptionResponseHeaders(source http.Header) map[string]string {
	headers := make(map[string]string, len(source))
	for name, values := range source {
		if len(values) == 0 {
			continue
		}
		headers[name] = values[0]
	}
	return headers
}

func transcriptionProviderResponseError(message string) *types.OpenAIErrorWithStatusCode {
	return &types.OpenAIErrorWithStatusCode{
		OpenAIError:      types.OpenAIError{Message: message, Type: "invalid_provider_response", Code: "invalid_provider_response"},
		StatusCode:       http.StatusBadGateway,
		UpstreamAccepted: true,
	}
}

func transcriptionAcceptedResponseError(err error, code string) *types.OpenAIErrorWithStatusCode {
	apiErr := common.ErrorWrapper(err, code, http.StatusBadGateway)
	apiErr.UpstreamAccepted = true
	return apiErr
}

func applyOpenAITranscriptionUsage(target *types.Usage, providerUsage *types.AudioUsage, actualModel string) {
	applyOpenAITranscriptionUsageForSource(target, providerUsage, actualModel, true)
}

func applyOpenAITranscriptionUsageForSource(target *types.Usage, providerUsage *types.AudioUsage, actualModel string, allowOperation bool) {
	if target == nil || providerUsage == nil {
		return
	}
	target.MergeProviderAttribution(actualModel, "")
	switch strings.ToLower(strings.TrimSpace(providerUsage.Type)) {
	case "duration":
		if providerUsage.Seconds != nil && validTranscriptionDuration(*providerUsage.Seconds) {
			target.SetProviderIndependentUsageUnit(config.UsageExtraInputAudioTranscription, *providerUsage.Seconds)
			if allowOperation {
				target.MarkProviderOperationUnits(1)
			}
		}
	case "tokens", "":
		if providerUsage.InputTokens == nil || providerUsage.OutputTokens == nil || providerUsage.TotalTokens == nil {
			return
		}
		target.PromptTokens = *providerUsage.InputTokens
		target.CompletionTokens = *providerUsage.OutputTokens
		target.TotalTokens = *providerUsage.TotalTokens
		target.ProviderTokenFields = map[string]bool{"prompt_tokens": true, "completion_tokens": true, "total_tokens": true}
		if target.PromptTokens+target.CompletionTokens != target.TotalTokens {
			target.ProviderTokenConflict = true
		}
		if providerUsage.InputDetails != nil {
			if providerUsage.InputDetails.TextTokens != nil {
				target.SetExtraTokens(config.UsageExtraInputTextTokens, *providerUsage.InputDetails.TextTokens)
				target.MarkProviderTokenField(config.UsageExtraInputTextTokens)
			}
			if providerUsage.InputDetails.AudioTokens != nil {
				target.SetExtraTokens(config.UsageExtraInputAudio, *providerUsage.InputDetails.AudioTokens)
				target.MarkProviderTokenField(config.UsageExtraInputAudio)
			}
		}
		target.RequireTokenExtraEvidence(config.UsageExtraInputTextTokens, config.UsageExtraInputAudio)
		target.SetTokenExtraEvidenceGroups([]string{config.UsageExtraInputTextTokens, config.UsageExtraInputAudio})
		target.MarkProviderReported()
	}
}

func validTranscriptionDuration(seconds float64) bool {
	return seconds >= 0 && !math.IsNaN(seconds) && !math.IsInf(seconds, 0)
}

func hasJSONResponse(request *types.AudioRequest) bool {
	return request.ResponseFormat == "" || request.ResponseFormat == "json" || request.ResponseFormat == "verbose_json"
}

func hasJSONTranscriptionResponse(request *types.AudioRequest) bool {
	return request != nil && (hasJSONResponse(request) || request.ResponseFormat == "diarized_json")
}

func (p *OpenAIProvider) getRequestAudioBody(relayMode int, ModelName string, request *types.AudioRequest) (*http.Request, *types.OpenAIErrorWithStatusCode) {
	url, errWithCode := p.GetSupportedAPIUri(relayMode)
	if errWithCode != nil {
		return nil, errWithCode
	}
	if errWithCode := p.validateAzureClassicAPIVersionForRequest(); errWithCode != nil {
		return nil, errWithCode
	}
	// 获取请求地址
	fullRequestURL := p.GetFullRequestURL(url, ModelName)

	// 获取请求头
	headers := p.GetRequestHeaders()
	body, exists, rawErr := p.nativeRawBody()
	if rawErr != nil {
		return nil, common.ErrorWrapperLocal(rawErr, "read_request_body_failed", http.StatusInternalServerError)
	}
	var req *http.Request
	var err error
	if exists {
		if p.Context == nil || p.Context.Request == nil {
			return nil, common.StringErrorWrapperLocal("request body not found", "request_body_not_found", http.StatusInternalServerError)
		}
		contentType := p.Context.Request.Header.Get("Content-Type")
		if p.OriginalModel != "" && p.OriginalModel != request.Model {
			body, contentType, rawErr = rewriteMultipartModel(body, contentType, request.Model)
			if rawErr != nil {
				return nil, common.ErrorWrapperLocal(rawErr, "invalid_multipart_request", http.StatusBadRequest)
			}
		}
		req, err = p.Requester.NewRequest(
			http.MethodPost,
			fullRequestURL,
			p.Requester.WithBody(body),
			p.Requester.WithHeader(headers),
			p.Requester.WithContentType(contentType))
		if req != nil {
			req.ContentLength = int64(len(body))
		}
	} else {
		var formBody bytes.Buffer
		builder := p.Requester.CreateFormBuilder(&formBody)
		if err := audioMultipartForm(request, builder); err != nil {
			return nil, common.ErrorWrapper(err, "create_form_builder_failed", http.StatusInternalServerError)
		}
		req, err = p.Requester.NewRequest(
			http.MethodPost,
			fullRequestURL,
			p.Requester.WithBody(&formBody),
			p.Requester.WithHeader(headers),
			p.Requester.WithContentType(builder.FormDataContentType()))
		if req != nil {
			req.ContentLength = int64(formBody.Len())
		}
	}

	if err != nil {
		return nil, common.ErrorWrapper(err, "new_request_failed", http.StatusInternalServerError)
	}
	if exists && p.usesNativeOpenAIWire() && p.Context != nil && p.Context.Request != nil {
		if err := p.applyOpenAIHTTPHeaders(req.Header, requestctx.NewHeaderSnapshot(p.Context.Request.Header)); err != nil {
			return nil, common.ErrorWrapperLocal(err, "invalid_request_header", http.StatusBadRequest)
		}
	}

	return req, nil
}

func rewriteMultipartModel(raw []byte, contentType, modelName string) ([]byte, string, error) {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || !strings.HasPrefix(strings.ToLower(mediaType), "multipart/") || strings.TrimSpace(params["boundary"]) == "" {
		return nil, "", fmt.Errorf("invalid multipart content type")
	}
	boundary := params["boundary"]
	reader := multipart.NewReader(bytes.NewReader(raw), boundary)
	var output bytes.Buffer
	writer := multipart.NewWriter(&output)
	if err := writer.SetBoundary(boundary); err != nil {
		return nil, "", err
	}
	foundModel := false
	for {
		part, nextErr := reader.NextRawPart()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			return nil, "", nextErr
		}
		headers := make(textproto.MIMEHeader, len(part.Header))
		for name, values := range part.Header {
			headers[name] = append([]string(nil), values...)
		}
		outputPart, createErr := writer.CreatePart(headers)
		if createErr != nil {
			_ = part.Close()
			return nil, "", createErr
		}
		if part.FormName() == "model" {
			foundModel = true
			_, err = io.WriteString(outputPart, modelName)
		} else {
			_, err = io.Copy(outputPart, part)
		}
		_ = part.Close()
		if err != nil {
			return nil, "", err
		}
	}
	if !foundModel {
		if err := writer.WriteField("model", modelName); err != nil {
			return nil, "", err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, "", err
	}
	return output.Bytes(), contentType, nil
}

func audioMultipartForm(request *types.AudioRequest, b requester.FormBuilder) error {
	err := b.CreateFormFile("file", request.File)
	if err != nil {
		return fmt.Errorf("creating form file: %w", err)
	}

	err = b.WriteField("model", request.Model)
	if err != nil {
		return fmt.Errorf("writing model name: %w", err)
	}

	if request.Prompt != "" {
		err = b.WriteField("prompt", request.Prompt)
		if err != nil {
			return fmt.Errorf("writing prompt: %w", err)
		}
	}

	if request.ResponseFormat != "" {
		err = b.WriteField("response_format", request.ResponseFormat)
		if err != nil {
			return fmt.Errorf("writing format: %w", err)
		}
	}

	if request.Temperature != 0 {
		err = b.WriteField("temperature", fmt.Sprintf("%.2f", request.Temperature))
		if err != nil {
			return fmt.Errorf("writing temperature: %w", err)
		}
	}

	if request.Language != "" {
		err = b.WriteField("language", request.Language)
		if err != nil {
			return fmt.Errorf("writing language: %w", err)
		}
	}

	return b.Close()
}

func getTextContent(text, format string) string {
	switch format {
	case "srt":
		return extractTextFromSRT(text)
	case "vtt":
		return extractTextFromVTT(text)
	default:
		return text
	}
}

func extractTextFromVTT(vttContent string) string {
	scanner := bufio.NewScanner(strings.NewReader(vttContent))
	re := regexp.MustCompile(`\d{2}:\d{2}:\d{2}\.\d{3} --> \d{2}:\d{2}:\d{2}\.\d{3}`)
	var text []string
	isStart := true

	for scanner.Scan() {
		line := scanner.Text()
		if isStart && strings.HasPrefix(line, "WEBVTT") {
			isStart = false
			continue
		}
		if !re.MatchString(line) && !isNumber(line) && line != "" {
			text = append(text, line)
		}
	}

	return strings.Join(text, " ")
}

func extractTextFromSRT(srtContent string) string {
	scanner := bufio.NewScanner(strings.NewReader(srtContent))
	re := regexp.MustCompile(`\d{2}:\d{2}:\d{2},\d{3} --> \d{2}:\d{2}:\d{2},\d{3}`)
	var text []string
	isContent := false

	for scanner.Scan() {
		line := scanner.Text()
		if re.MatchString(line) {
			isContent = true
		} else if line == "" {
			isContent = false
		} else if isContent {
			text = append(text, line)
		}
	}

	return strings.Join(text, " ")
}

func isNumber(s string) bool {
	_, err := strconv.Atoi(s)
	return err == nil
}
