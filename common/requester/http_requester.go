package requester

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"one-api/common"
	"one-api/common/logger"
	"one-api/common/providerresponse"
	"one-api/common/requestctx"
	"one-api/common/utils"
	"one-api/types"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
)

type HttpErrorHandler func(*http.Response) *types.OpenAIError

const maxProviderErrorBodyBytes int64 = 1 << 20
const maxProviderRawJSONBodyBytes int64 = 64 << 20

var errProviderRawJSONBodyTooLarge = fmt.Errorf("provider response body exceeds %d bytes", maxProviderRawJSONBodyBytes)

// ProviderRawJSON captures the provider response before adapters normalize it.
// Capturing raw JSON does not by itself authorize replaying it to the client.
type ProviderRawJSON interface {
	SetProviderRawJSON([]byte)
	ProviderRawJSON() []byte
}

// ProviderRawJSONCapturer is the explicit buffering opt-in. Merely
// implementing ProviderRawJSON must not turn every successful response into
// an unbounded in-memory replay buffer.
type ProviderRawJSONCapturer interface {
	ProviderRawJSON
	CaptureProviderRawJSON() bool
}

// ProviderRawJSONDecoder lets an exact-wire response extract only the stable
// evidence the proxy owns without making successful delivery depend on every
// provider-owned response union fitting the current DTO.
type ProviderRawJSONDecoder interface {
	DecodeCapturedProviderJSON([]byte) error
}

// ProviderRawJSONReplayer is the explicit exact-wire delivery contract.
// Adapters must opt in only after all response processing is complete.
type ProviderRawJSONReplayer interface {
	ReplayProviderRawJSON() []byte
}

type HTTPRequester struct {
	// ObserveRequest 仅发布实际发送请求的事实，不执行协议策略。
	ObserveRequest func(*http.Request)
	// requestBuilder    utils.RequestBuilder
	CreateFormBuilder          func(io.Writer) FormBuilder
	ErrorHandler               HttpErrorHandler
	proxyAddr                  string
	Context                    context.Context
	PrefixProviderErrors       bool
	ReplayOpenAIErrorEnvelopes bool
	profile                    HTTPProfile
	transports                 *providerHTTPTransportSet
}

// NewHTTPRequester 创建一个新的 HTTPRequester 实例。
// proxyAddr: 是代理服务器的地址。
// errorHandler: 是一个错误处理函数，它接收一个 *http.Response 参数并返回一个 *types.OpenAIErrorResponse。
// 如果 errorHandler 为 nil，那么会使用一个默认的错误处理函数。
func NewHTTPRequester(proxyAddr string, errorHandler HttpErrorHandler) *HTTPRequester {
	return &HTTPRequester{
		CreateFormBuilder: func(body io.Writer) FormBuilder {
			return NewFormBuilder(body)
		},
		ErrorHandler:         errorHandler,
		proxyAddr:            proxyAddr,
		Context:              context.Background(),
		PrefixProviderErrors: true,
		profile:              HTTPProfileWorkAction,
		transports:           providerHTTPTransportsForCurrentClient(),
	}
}

func providerHTTPTransportsForCurrentClient() *providerHTTPTransportSet {
	if HTTPClient == nil {
		return defaultProviderHTTPTransports
	}
	source := HTTPClient.Transport
	if source == nil {
		source = http.DefaultTransport
	}
	transport, ok := source.(*http.Transport)
	if !ok {
		return nil
	}
	if defaultProviderHTTPTransports != nil && defaultProviderHTTPTransports.normal == transport {
		return defaultProviderHTTPTransports
	}
	return newProviderHTTPTransportSet(transport)
}

// UseHTTPProfile selects one code-owned transport policy for subsequent
// requests made by this requester. It does not change protocol parsing.
func (r *HTTPRequester) UseHTTPProfile(profile HTTPProfile) {
	if r == nil {
		return
	}
	r.profile = profile
}

// ForHTTPProfile returns a request-local view of an already configured
// requester. Adapters with multi-stage workflows can therefore apply a long
// stream policy only to the request that actually opens the stream. As with
// ordinary struct reads, callers must not mutate the base concurrently.
func (r *HTTPRequester) ForHTTPProfile(profile HTTPProfile) *HTTPRequester {
	if r == nil {
		return nil
	}
	cloned := *r
	cloned.profile = profile
	return &cloned
}

type requestOptions struct {
	body   any
	header http.Header
	ctx    context.Context
}

type requestOption func(*requestOptions)

func (r *HTTPRequester) requestContext(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = r.Context
	}
	return utils.SetProxy(r.proxyAddr, ctx)
}

func doHTTPRequest(client *http.Client, req *http.Request, policy httpPolicy, noKeepAlive ...*http.Transport) (*http.Response, error, bool) {
	if client == nil || req == nil {
		return nil, errors.New("http client and request are required"), false
	}

	requestCtx := req.Context()
	var cancel context.CancelFunc
	if policy.maxLifetime > 0 {
		requestCtx, cancel = context.WithTimeout(requestCtx, policy.maxLifetime)
	} else if policy.responseHeaderTimeout > 0 || policy.bodyIdleTimeout > 0 {
		requestCtx, cancel = context.WithCancel(requestCtx)
	}

	var wroteRequest atomic.Bool
	headerDeadline := newResponseHeaderDeadline(policy.responseHeaderTimeout, cancel)
	trace := &httptrace.ClientTrace{WroteRequest: func(httptrace.WroteRequestInfo) {
		wroteRequest.Store(true)
		headerDeadline.Start()
	}}
	// 当前 requester 调用方没有为非安全方法声明上游幂等合同。保留安全读方法
	// 的 net/http 既有重试路径，但不能让客户端 Idempotency-Key 把自动重建的
	// 工作请求体变成第二次提交。
	transportReq := requestWithoutImplicitReplay(req)
	if requestNeedsNoKeepAlives(req) {
		if len(noKeepAlive) == 0 || noKeepAlive[0] == nil {
			if cancel != nil {
				cancel()
			}
			return nil, errNoImplicitReplayTransport, false
		}
		clientCopy := *client
		clientCopy.Transport = noKeepAlive[0]
		client = &clientCopy
	}
	traced := transportReq.WithContext(httptrace.WithClientTrace(requestCtx, trace))
	resp, err := client.Do(traced)
	headerDeadline.Stop()
	if err != nil {
		if cancel != nil {
			cancel()
		}
		return resp, err, wroteRequest.Load()
	}
	if cancel != nil {
		if resp == nil || resp.Body == nil {
			cancel()
		} else {
			resp.Body = newPolicyResponseBody(resp.Body, requestCtx, cancel, policy.bodyIdleTimeout)
		}
	}
	return resp, err, wroteRequest.Load()
}

func requestWithoutImplicitReplay(req *http.Request) *http.Request {
	if req == nil || safeTransportRetryMethod(req.Method) {
		return req
	}

	bodyPresent := req.Body != nil && req.Body != http.NoBody
	if bodyPresent && req.GetBody != nil {
		cloned := req.Clone(req.Context())
		cloned.GetBody = nil
		return cloned
	}

	return req
}

func requestNeedsNoKeepAlives(req *http.Request) bool {
	return req != nil && !safeTransportRetryMethod(req.Method) &&
		(req.Body == nil || req.Body == http.NoBody)
}

func safeTransportRetryMethod(method string) bool {
	switch method {
	case "", http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	default:
		return false
	}
}

type responseHeaderDeadline struct {
	mu      sync.Mutex
	timeout time.Duration
	cancel  context.CancelFunc
	timer   *time.Timer
	done    bool
}

func newResponseHeaderDeadline(timeout time.Duration, cancel context.CancelFunc) *responseHeaderDeadline {
	return &responseHeaderDeadline{timeout: timeout, cancel: cancel}
}

func (d *responseHeaderDeadline) Start() {
	if d == nil || d.timeout <= 0 || d.cancel == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.done || d.timer != nil {
		return
	}
	d.timer = time.AfterFunc(d.timeout, d.cancel)
}

func (d *responseHeaderDeadline) Stop() {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.done = true
	if d.timer != nil {
		d.timer.Stop()
	}
	d.mu.Unlock()
}

type policyResponseBody struct {
	body       io.ReadCloser
	cancel     context.CancelFunc
	idle       time.Duration
	stopCancel func() bool
	closeOnce  sync.Once
	closeError error
}

func newPolicyResponseBody(body io.ReadCloser, requestCtx context.Context, cancel context.CancelFunc, idle time.Duration) io.ReadCloser {
	wrapped := &policyResponseBody{body: body, cancel: cancel, idle: idle}
	if requestCtx != nil {
		wrapped.stopCancel = context.AfterFunc(requestCtx, func() {
			wrapped.close(false)
		})
	}
	return wrapped
}

func (b *policyResponseBody) Read(p []byte) (int, error) {
	if b == nil || b.body == nil {
		return 0, io.EOF
	}
	var idleTimer *time.Timer
	if b.idle > 0 {
		idleTimer = time.AfterFunc(b.idle, func() {
			_ = b.Close()
		})
	}
	n, err := b.body.Read(p)
	idleExpired := false
	if idleTimer != nil {
		idleExpired = !idleTimer.Stop()
	}
	if err != nil && !idleExpired {
		_ = b.Close()
	}
	return n, err
}

func (b *policyResponseBody) Close() error {
	return b.close(true)
}

func (b *policyResponseBody) close(stopCancelObserver bool) error {
	if b == nil {
		return nil
	}
	b.closeOnce.Do(func() {
		if stopCancelObserver && b.stopCancel != nil {
			b.stopCancel()
		}
		if b.cancel != nil {
			b.cancel()
		}
		if b.body != nil {
			b.closeError = b.body.Close()
		}
	})
	return b.closeError
}

func httpTransportError(err error, wroteRequest bool) *types.OpenAIErrorWithStatusCode {
	if err != nil {
		logger.SysError(fmt.Sprintf("provider http transport error: %s", err.Error()))
	}
	apiErr := common.StringErrorWrapper("请求上游地址失败", "http_request_failed", http.StatusInternalServerError)
	if wroteRequest {
		apiErr.UpstreamAmbiguous = true
	} else {
		apiErr.UpstreamNotAttempted = true
	}
	return apiErr
}

// 创建请求
func (r *HTTPRequester) NewRequest(method, url string, setters ...requestOption) (*http.Request, error) {
	args := &requestOptions{
		body:   nil,
		header: make(http.Header),
	}
	for _, setter := range setters {
		setter(args)
	}
	req, err := utils.RequestBuilder(r.requestContext(args.ctx), method, url, args.body, args.header)
	if err != nil {
		return nil, err
	}

	return req, nil
}

// 发送请求
func (r *HTTPRequester) SendRequest(req *http.Request, response any, outputResp bool) (*http.Response, *types.OpenAIErrorWithStatusCode) {
	return r.sendRequest(req, response, outputResp, false, r.ReplayOpenAIErrorEnvelopes)
}

// SendRequestPreservingRedirect keeps an un-followed provider 3xx available
// for an explicitly authorized exact-wire response surface.
func (r *HTTPRequester) SendRequestPreservingRedirect(req *http.Request, response any, outputResp bool) (*http.Response, *types.OpenAIErrorWithStatusCode) {
	return r.sendRequest(req, response, outputResp, true, r.ReplayOpenAIErrorEnvelopes)
}

// SendRequestPreservingNativeDialect keeps a native provider's safe error and
// redirect wire without changing the requester's behavior for cross-protocol
// consumers that share the same configured channel.
func (r *HTTPRequester) SendRequestPreservingNativeDialect(req *http.Request, response any, outputResp bool) (*http.Response, *types.OpenAIErrorWithStatusCode) {
	return r.sendRequest(req, response, outputResp, true, true)
}

func (r *HTTPRequester) sendRequest(req *http.Request, response any, outputResp bool, preserveRedirect bool, replayProviderEnvelope bool) (*http.Response, *types.OpenAIErrorWithStatusCode) {
	client, noKeepAlive, clientErr := r.configuredHTTPClient(r.profile, preserveRedirect)
	if clientErr != nil {
		return nil, clientErr
	}
	if r.ObserveRequest != nil {
		r.ObserveRequest(req)
	}
	resp, err, wroteRequest := doHTTPRequest(client, req, policyForHTTPProfile(r.profile), noKeepAlive)
	if err != nil {
		return nil, httpTransportError(err, wroteRequest)
	}

	if !outputResp {
		defer resp.Body.Close()
	}

	// 处理响应
	if r.IsFailureStatusCode(resp) {
		if preserveRedirect && isRedirectStatus(resp.StatusCode) {
			return nil, preservedRedirectResponse(resp, providerresponse.OperationUnknown)
		}
		apiErr := HandleErrorResp(resp, r.ErrorHandler, r.PrefixProviderErrors, replayProviderEnvelope)
		if apiErr != nil && (resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode >= http.StatusInternalServerError) {
			apiErr.UpstreamAmbiguous = true
		}
		return nil, apiErr
	}

	// 解析响应
	if response == nil {
		return resp, nil
	}

	if outputResp {
		var buf bytes.Buffer
		originalBody := resp.Body
		tee := io.TeeReader(originalBody, &buf)
		err = DecodeResponse(tee, response)
		closeErr := originalBody.Close()

		// 将响应体重新写入 resp.Body
		resp.Body = io.NopCloser(bytes.NewReader(buf.Bytes()))
		if err == nil {
			err = closeErr
		}
	} else if rawResponse, ok := response.(ProviderRawJSONCapturer); ok && rawResponse.CaptureProviderRawJSON() {
		var body []byte
		body, err = readProviderBodyBounded(resp.Body, maxProviderRawJSONBodyBytes)

		if err == nil {
			if decoder, ok := response.(ProviderRawJSONDecoder); ok {
				err = decoder.DecodeCapturedProviderJSON(body)
			} else {
				err = DecodeResponse(bytes.NewReader(body), response)
			}
		}
		if err == nil {
			rawResponse.SetProviderRawJSON(body)
		}
	} else {
		err = json.NewDecoder(resp.Body).Decode(response)
	}

	if err != nil {
		apiErr := common.ErrorWrapper(err, "decode_response_failed", http.StatusInternalServerError)
		if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
			apiErr.UpstreamAccepted = true
		}
		apiErr.ResponseHeaders = requestctx.SafeProviderResponseHeaders(resp.Header, providerresponse.RequestCredentials(resp.Request)...)
		return nil, apiErr
	}

	return resp, nil
}

func readProviderBodyBounded(reader io.Reader, maxBytes int64) ([]byte, error) {
	if reader == nil || maxBytes <= 0 {
		return nil, errors.New("provider response body limit is invalid")
	}
	body, err := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxBytes {
		return nil, errProviderRawJSONBodyTooLarge
	}
	return body, nil
}

// 发送请求 RAW
func (r *HTTPRequester) SendRequestRaw(req *http.Request) (*http.Response, *types.OpenAIErrorWithStatusCode) {
	// 发送请求
	client, noKeepAlive, clientErr := r.configuredHTTPClient(r.profile, false)
	if clientErr != nil {
		return nil, clientErr
	}
	if r.ObserveRequest != nil {
		r.ObserveRequest(req)
	}
	resp, err, wroteRequest := doHTTPRequest(client, req, policyForHTTPProfile(r.profile), noKeepAlive)
	if err != nil {
		return nil, httpTransportError(err, wroteRequest)
	}

	// 处理响应
	if r.IsFailureStatusCode(resp) {
		apiErr := HandleErrorResp(resp, r.ErrorHandler, r.PrefixProviderErrors, r.ReplayOpenAIErrorEnvelopes)
		if apiErr != nil && (resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode >= http.StatusInternalServerError) {
			apiErr.UpstreamAmbiguous = true
		}
		return nil, apiErr
	}

	return resp, nil
}

// SendRequestRawNoRedirect is reserved for explicit raw relay surfaces. It
// returns every HTTP status unchanged and prevents the proxy from turning an
// upstream redirect into a second request with different authority semantics.
func (r *HTTPRequester) SendRequestRawNoRedirect(req *http.Request) (*http.Response, *types.OpenAIErrorWithStatusCode) {
	return r.sendRequestRawNoRedirect(req)
}

// SendRequestRawCheckedNoRedirect prevents authority-changing redirects while
// keeping provider failures behind the shared bounded redaction policy.
func (r *HTTPRequester) SendRequestRawCheckedNoRedirect(req *http.Request) (*http.Response, *types.OpenAIErrorWithStatusCode) {
	return r.sendRequestRawCheckedNoRedirect(req, false, providerresponse.OperationUnknown)
}

// SendRequestRawCheckedPreservingRedirect applies the normal bounded provider
// error policy while retaining an explicitly authorized 3xx status, safe
// headers, and body for exact-wire downstream delivery.
func (r *HTTPRequester) SendRequestRawCheckedPreservingRedirect(req *http.Request, operation providerresponse.Operation) (*http.Response, *types.OpenAIErrorWithStatusCode) {
	return r.sendRequestRawCheckedNoRedirect(req, true, operation)
}

func (r *HTTPRequester) SendRequestRawCheckedNativeDialect(req *http.Request, operation providerresponse.Operation) (*http.Response, *types.OpenAIErrorWithStatusCode) {
	resp, errWithCode := r.sendRequestRawNoRedirect(req)
	if errWithCode != nil {
		return nil, errWithCode
	}
	if isRedirectStatus(resp.StatusCode) {
		return nil, preservedRedirectResponse(resp, operation)
	}
	if r.IsFailureStatusCode(resp) {
		return nil, HandleErrorResp(resp, r.ErrorHandler, r.PrefixProviderErrors, true)
	}
	return resp, nil
}

func (r *HTTPRequester) sendRequestRawCheckedNoRedirect(req *http.Request, preserveRedirect bool, operation providerresponse.Operation) (*http.Response, *types.OpenAIErrorWithStatusCode) {
	resp, errWithCode := r.sendRequestRawNoRedirect(req)
	if errWithCode != nil {
		return nil, errWithCode
	}
	if req != nil && req.Method == http.MethodGet && resp.StatusCode == http.StatusNotModified {
		return resp, nil
	}
	if preserveRedirect && isRedirectStatus(resp.StatusCode) {
		return nil, preservedRedirectResponse(resp, operation)
	}
	if r.IsFailureStatusCode(resp) {
		return nil, HandleErrorResp(resp, r.ErrorHandler, r.PrefixProviderErrors, r.ReplayOpenAIErrorEnvelopes)
	}
	return resp, nil
}

func isRedirectStatus(statusCode int) bool {
	return statusCode >= http.StatusMultipleChoices && statusCode < http.StatusBadRequest
}

func preservedRedirectResponse(resp *http.Response, operation providerresponse.Operation) *types.OpenAIErrorWithStatusCode {
	if resp == nil {
		return common.StringErrorWrapper("provider redirect response is missing", "invalid_provider_response", http.StatusBadGateway)
	}
	defer resp.Body.Close()
	body, err := readProviderBodyBounded(resp.Body, maxProviderRawJSONBodyBytes)
	if err != nil {
		apiErr := common.ErrorWrapperLocal(err, "invalid_provider_response", http.StatusBadGateway)
		apiErr.UpstreamAccepted = true
		return apiErr
	}
	headers := providerresponse.Filter(resp.Header, providerresponse.Policy{
		Operation: operation, DataPath: providerresponse.DataPathExactWire,
		BodyUnmodified: true, PreserveRedirect: true,
	})
	return &types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{
			Message: "provider returned an HTTP redirect",
			Type:    "upstream_response",
			Code:    "provider_redirect_response",
		},
		StatusCode:        resp.StatusCode,
		ReplayRawResponse: true,
		RawBody:           body,
		ResponseHeaders:   headers,
	}
}

func (r *HTTPRequester) sendRequestRawNoRedirect(req *http.Request) (*http.Response, *types.OpenAIErrorWithStatusCode) {
	client, noKeepAlive, clientErr := r.configuredHTTPClient(r.profile, true)
	if clientErr != nil {
		return nil, clientErr
	}
	if r.ObserveRequest != nil {
		r.ObserveRequest(req)
	}
	resp, err, wroteRequest := doHTTPRequest(client, req, policyForHTTPProfile(r.profile), noKeepAlive)
	if err != nil {
		return nil, httpTransportError(err, wroteRequest)
	}
	return resp, nil
}

func providerHTTPClient(profile HTTPProfile, forceNoRedirect bool) (*http.Client, *types.OpenAIErrorWithStatusCode) {
	if HTTPClient == nil {
		return nil, common.StringErrorWrapperLocal("http client is not initialized", "http_request_failed", http.StatusInternalServerError)
	}
	client := *HTTPClient
	policy := policyForHTTPProfile(profile)
	if forceNoRedirect {
		policy.redirect = redirectReject
	}
	client.CheckRedirect = checkRedirectForPolicy(policy)
	if policy.overrideTimeout {
		client.Timeout = policy.overallTimeout
	}
	return &client, nil
}

func (r *HTTPRequester) configuredHTTPClient(profile HTTPProfile, forceNoRedirect bool) (*http.Client, *http.Transport, *types.OpenAIErrorWithStatusCode) {
	client, apiErr := providerHTTPClient(profile, forceNoRedirect)
	if apiErr != nil {
		return nil, nil, apiErr
	}
	if r == nil || r.transports == nil {
		return client, nil, nil
	}
	source := client.Transport
	if source == nil {
		source = http.DefaultTransport
	}
	transport, _ := r.transports.noKeepAliveFor(source)
	return client, transport, nil
}

// 获取流式响应
func RequestStream[T streamable](requester *HTTPRequester, resp *http.Response, handlerPrefix HandlerPrefix[T]) (*streamReader[T], *types.OpenAIErrorWithStatusCode) {
	return RequestStreamWithOptions(requester, resp, handlerPrefix, StreamReadOptions{})
}

func RequestStreamWithOptions[T streamable](requester *HTTPRequester, resp *http.Response, handlerPrefix HandlerPrefix[T], options StreamReadOptions) (*streamReader[T], *types.OpenAIErrorWithStatusCode) {
	// 如果返回的头是json格式 说明有错误
	// if strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
	// 	return nil, HandleErrorResp(resp, requester.ErrorHandler, requester.PrefixProviderErrors, requester.ReplayOpenAIErrorEnvelopes)
	// }

	readContext, readEnded := context.WithCancelCause(context.Background())
	stream := &streamReader[T]{
		readContext:   readContext,
		readEnded:     readEnded,
		reader:        bufio.NewReader(resp.Body),
		response:      resp,
		handlerPrefix: handlerPrefix,
		NoTrim:        false,
		options:       normalizeStreamReadOptions(options),

		// Keep data unbuffered so terminal errors cannot overtake emitted chunks.
		DataChan: make(chan T),
		ErrChan:  make(chan error, 1),
		done:     make(chan struct{}),
	}

	return stream, nil
}

func RequestStreamWithEmitterOptions[T streamable](requester *HTTPRequester, resp *http.Response, handlerPrefix HandlerPrefixWithEmitter[T], options StreamReadOptions) (*streamReader[T], *types.OpenAIErrorWithStatusCode) {
	stream, errWithCode := RequestStreamWithOptions[T](requester, resp, nil, options)
	if errWithCode != nil {
		return nil, errWithCode
	}
	stream.handlerPrefixEmitter = handlerPrefix
	return stream, nil
}

func RequestNoTrimStream[T streamable](requester *HTTPRequester, resp *http.Response, handlerPrefix HandlerPrefix[T]) (*streamReader[T], *types.OpenAIErrorWithStatusCode) {
	return RequestNoTrimStreamWithOptions(requester, resp, handlerPrefix, StreamReadOptions{})
}

func RequestNoTrimStreamWithOptions[T streamable](requester *HTTPRequester, resp *http.Response, handlerPrefix HandlerPrefix[T], options StreamReadOptions) (*streamReader[T], *types.OpenAIErrorWithStatusCode) {
	stream, err := RequestStreamWithOptions(requester, resp, handlerPrefix, options)
	if err != nil {
		return nil, err
	}

	stream.NoTrim = true

	return stream, nil
}

func RequestNoTrimStreamWithEmitterOptions[T streamable](requester *HTTPRequester, resp *http.Response, handlerPrefix HandlerPrefixWithEmitter[T], options StreamReadOptions) (*streamReader[T], *types.OpenAIErrorWithStatusCode) {
	stream, err := RequestStreamWithEmitterOptions(requester, resp, handlerPrefix, options)
	if err != nil {
		return nil, err
	}

	stream.NoTrim = true

	return stream, nil
}

// 设置请求体
func (r *HTTPRequester) WithBody(body any) requestOption {
	return func(args *requestOptions) {
		args.body = body
	}
}

// WithContext sets the request context while preserving requester-owned
// transport context values such as proxy configuration.
func (r *HTTPRequester) WithContext(ctx context.Context) requestOption {
	return func(args *requestOptions) {
		args.ctx = ctx
	}
}

func (r *HTTPRequester) WithRequestContext(req *http.Request, ctx context.Context) *http.Request {
	if req == nil || ctx == nil {
		return req
	}
	return req.WithContext(r.requestContext(ctx))
}

// 设置请求头
func (r *HTTPRequester) WithHeader(header map[string]string) requestOption {
	return func(args *requestOptions) {
		for k, v := range header {
			args.header.Set(k, v)
		}
	}
}

// 设置Content-Type
func (r *HTTPRequester) WithContentType(contentType string) requestOption {
	return func(args *requestOptions) {
		args.header.Set("Content-Type", contentType)
	}
}

// 判断是否为失败状态码
func (r *HTTPRequester) IsFailureStatusCode(resp *http.Response) bool {
	return resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices
}

// 处理错误响应
func HandleErrorResp(resp *http.Response, toOpenAIError HttpErrorHandler, prefixProviderError bool, replayOpenAIEnvelope bool) *types.OpenAIErrorWithStatusCode {
	providerSecrets := providerresponse.RequestCredentials(resp.Request)
	openAIErrorWithStatusCode := &types.OpenAIErrorWithStatusCode{
		StatusCode:             resp.StatusCode,
		ResponseHeaders:        requestctx.SafeProviderResponseHeaders(resp.Header, providerSecrets...),
		ProviderQuotaExhausted: resp.StatusCode == http.StatusPaymentRequired,
		ProviderAuthRejected:   resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusProxyAuthRequired,
		ProviderRateLimited:    resp.StatusCode == http.StatusTooManyRequests,
		OpenAIError: types.OpenAIError{
			Message: "",
			Type:    "upstream_error",
			Code:    "bad_response_status_code",
			Param:   strconv.Itoa(resp.StatusCode),
		},
	}

	defer resp.Body.Close()

	if toOpenAIError != nil {
		bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, maxProviderErrorBodyBytes+1))
		if err == nil && int64(len(bodyBytes)) <= maxProviderErrorBodyBytes {
			resp.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
			errorResponse := toOpenAIError(resp)

			if errorResponse != nil && errorResponse.Message != "" {
				if strings.HasPrefix(errorResponse.Message, "当前分组") {
					openAIErrorWithStatusCode.StatusCode = http.StatusTooManyRequests
				}

				openAIErrorWithStatusCode.OpenAIError = *errorResponse
				quotaExhausted := common.ProviderErrorIsQuotaExhausted(*errorResponse) ||
					(!common.ProviderErrorIsRateLimited(*errorResponse) && strings.Contains(errorResponse.Message, "Your credit balance is too low"))
				authRejected := common.ProviderErrorIsAuthRejected(*errorResponse) ||
					(errorResponse.Param == "INVALID_ARGUMENT" && strings.Contains(errorResponse.Message, "API key not valid"))
				openAIErrorWithStatusCode.ProviderQuotaExhausted = openAIErrorWithStatusCode.ProviderQuotaExhausted || quotaExhausted
				openAIErrorWithStatusCode.ProviderAuthRejected = openAIErrorWithStatusCode.ProviderAuthRejected || authRejected
				openAIErrorWithStatusCode.ProviderRateLimited = openAIErrorWithStatusCode.ProviderRateLimited || common.ProviderErrorIsRateLimited(*errorResponse)
				if replayOpenAIEnvelope {
					var envelope map[string]json.RawMessage
					var errorObject map[string]json.RawMessage
					if json.Unmarshal(bodyBytes, &envelope) == nil && json.Unmarshal(envelope["error"], &errorObject) == nil && errorObject != nil {
						openAIErrorWithStatusCode.RawBody = bodyBytes
						openAIErrorWithStatusCode.ReplayRawResponse = true
					}
				}

				if quotaExhausted && !replayOpenAIEnvelope {
					openAIErrorWithStatusCode.StatusCode = http.StatusTooManyRequests
				}
				if prefixProviderError {
					openAIErrorWithStatusCode.OpenAIError.Message = fmt.Sprintf("Provider API error: %s", openAIErrorWithStatusCode.OpenAIError.Message)
				}
			}

			// If the provider-specific mapper cannot produce a normalized error,
			// keep the generic status summary below. Raw JSON bodies can contain
			// account or credential details and must not be surfaced to clients.
		}
	}

	if openAIErrorWithStatusCode.OpenAIError.Message == "" {
		if prefixProviderError {
			openAIErrorWithStatusCode.OpenAIError.Message = fmt.Sprintf("Provider API error: bad response status code %d", resp.StatusCode)
		} else {
			openAIErrorWithStatusCode.OpenAIError.Message = fmt.Sprintf("bad response status code %d", resp.StatusCode)
		}
	}

	return openAIErrorWithStatusCode
}

func SetEventStreamHeaders(c *gin.Context) {
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("Transfer-Encoding", "chunked")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
}

func GetJsonHeaders() map[string]string {
	return map[string]string{
		"Content-type": "application/json",
	}
}

type Stringer interface {
	GetString() *string
}

func DecodeResponse(body io.Reader, v any) error {
	if v == nil {
		return nil
	}

	if result, ok := v.(*string); ok {
		return DecodeString(body, result)
	}

	if stringer, ok := v.(Stringer); ok {
		return DecodeString(body, stringer.GetString())
	}

	return json.NewDecoder(body).Decode(v)
}

func DecodeString(body io.Reader, output *string) error {
	b, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	*output = string(b)
	return nil
}
