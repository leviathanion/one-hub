package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"one-api/common"
	cfgpkg "one-api/common/config"
	"one-api/common/logger"
	"one-api/common/requester"
	"one-api/model"
	baseprovider "one-api/providers/base"
	"one-api/providers/openai"
	"one-api/types"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
)

const (
	benchRequestParseKey   = "request_parse_ms"
	benchProviderSelectKey = "provider_select_ms"
	benchPromptCountKey    = "prompt_count_ms"
	benchUpstreamHeaderKey = "upstream_header_ms"
	benchTTFTKey           = "ttft_ms"
	benchTransformModeKey  = "transform_mode"
	benchCachedBodyKey     = "bench_cached_request_body"
)

type selfHostedBench struct {
	transport http.RoundTripper
	metrics   *benchMetrics
}

func (b *selfHostedBench) Close() {
}

func startSelfHostedBench(cfg *config) (*selfHostedBench, error) {
	gin.SetMode(gin.ReleaseMode)
	if logger.Logger == nil {
		logger.Logger = zap.NewNop()
	}
	cfgpkg.DisableTokenEncoders = true

	channel := buildBenchChannel(cfg.model)
	benchMetrics := newBenchMetrics()
	transport := &inMemoryTransport{
		handlers: map[string]http.Handler{
			"target.local":   newBenchTargetRouter("http://upstream.local", channel, benchMetrics),
			"upstream.local": newMockUpstreamHandler(),
		},
	}
	requester.HTTPClient = &http.Client{Transport: transport}

	cfg.baseURL = "http://target.local"
	cfg.metricsURL = "http://target.local/api/metrics"
	cfg.metricsUser = ""
	cfg.metricsPassword = ""
	cfg.apiKey = "self-hosted-bench"

	return &selfHostedBench{
		transport: transport,
		metrics:   benchMetrics,
	}, nil
}

type inMemoryTransport struct {
	handlers map[string]http.Handler
}

func (t *inMemoryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	handler, ok := t.handlers[req.URL.Host]
	if !ok {
		return nil, fmt.Errorf("no in-memory handler for host %s", req.URL.Host)
	}

	serverReq := req.Clone(req.Context())
	serverReq.RequestURI = req.URL.RequestURI()
	if req.Body != nil {
		bodyBytes, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		_ = req.Body.Close()
		req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		serverReq.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, serverReq)
	return recorder.Result(), nil
}

func buildBenchChannel(modelName string) *model.Channel {
	weight := uint(1)
	priority := int64(1)
	baseURL := ""
	proxy := ""
	modelHeaders := `{"x-bench":"1"}`
	modelMapping := fmt.Sprintf(`{"%s":"bench-%s"}`, modelName, modelName)
	customParameter := `{"pre_add":true,"metadata":{"bench":"true"},"temperature":0.25}`
	channel := &model.Channel{
		Id:              1,
		Type:            cfgpkg.ChannelTypeOpenAI,
		Key:             "bench-upstream-key",
		Name:            "bench-channel",
		Group:           "default",
		Models:          modelName,
		BaseURL:         &baseURL,
		Proxy:           &proxy,
		Weight:          &weight,
		Priority:        &priority,
		ModelHeaders:    &modelHeaders,
		ModelMapping:    &modelMapping,
		CustomParameter: &customParameter,
		AllowExtraBody:  true,
		PreCost:         1,
	}

	model.PricingInstance = &model.Pricing{
		Prices: map[string]*model.Price{
			modelName: {
				Model:       modelName,
				Type:        model.TokensPriceType,
				ChannelType: cfgpkg.ChannelTypeOpenAI,
				Input:       0,
				Output:      0,
			},
			"bench-" + modelName: {
				Model:       "bench-" + modelName,
				Type:        model.TokensPriceType,
				ChannelType: cfgpkg.ChannelTypeOpenAI,
				Input:       0,
				Output:      0,
			},
		},
	}
	model.GlobalUserGroupRatio = model.UserGroupRatio{
		UserGroup: map[string]*model.UserGroup{
			"default": {
				Id:      1,
				Symbol:  "default",
				Name:    "Default",
				Ratio:   1,
				APIRate: 0,
				Public:  true,
			},
		},
		PublicGroup: []string{"default"},
	}
	return channel
}

type benchMetrics struct {
	registry         *prometheus.Registry
	requestParseMS   *prometheus.HistogramVec
	providerSelectMS *prometheus.HistogramVec
	promptCountMS    *prometheus.HistogramVec
	upstreamHeaderMS *prometheus.HistogramVec
	ttftMS           *prometheus.HistogramVec
}

func newBenchMetrics() *benchMetrics {
	registry := prometheus.NewRegistry()
	labels := []string{"path", "transform_mode"}
	buckets := []float64{1, 2, 5, 10, 20, 50, 100, 200, 500, 1000}

	m := &benchMetrics{
		registry: registry,
		requestParseMS: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    benchRequestParseKey,
			Help:    "Self-hosted bench request parse milliseconds.",
			Buckets: buckets,
		}, labels),
		providerSelectMS: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    benchProviderSelectKey,
			Help:    "Self-hosted bench provider selection milliseconds.",
			Buckets: buckets,
		}, labels),
		promptCountMS: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    benchPromptCountKey,
			Help:    "Self-hosted bench prompt count milliseconds.",
			Buckets: buckets,
		}, labels),
		upstreamHeaderMS: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    benchUpstreamHeaderKey,
			Help:    "Self-hosted bench upstream header milliseconds.",
			Buckets: buckets,
		}, labels),
		ttftMS: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    benchTTFTKey,
			Help:    "Self-hosted bench TTFT milliseconds.",
			Buckets: buckets,
		}, labels),
	}

	registry.MustRegister(m.requestParseMS, m.providerSelectMS, m.promptCountMS, m.upstreamHeaderMS, m.ttftMS)
	return m
}

func (m *benchMetrics) observe(c *gin.Context) {
	if m == nil {
		return
	}
	path := c.Request.URL.Path
	transformMode := c.GetString(benchTransformModeKey)
	if transformMode == "" {
		transformMode = "unknown"
	}
	labels := []string{path, transformMode}

	if value := c.GetInt64(benchRequestParseKey); value > 0 {
		m.requestParseMS.WithLabelValues(labels...).Observe(float64(value))
	}
	if value := c.GetInt64(benchProviderSelectKey); value > 0 {
		m.providerSelectMS.WithLabelValues(labels...).Observe(float64(value))
	}
	if value := c.GetInt64(benchPromptCountKey); value > 0 {
		m.promptCountMS.WithLabelValues(labels...).Observe(float64(value))
	}
	if value := c.GetInt64(benchUpstreamHeaderKey); value > 0 {
		m.upstreamHeaderMS.WithLabelValues(labels...).Observe(float64(value))
	}
	if value := c.GetInt64(benchTTFTKey); value > 0 {
		m.ttftMS.WithLabelValues(labels...).Observe(float64(value))
	}
}

func newBenchTargetRouter(upstreamURL string, channel *model.Channel, benchMetrics *benchMetrics) http.Handler {
	router := gin.New()
	router.POST("/v1/chat/completions", func(c *gin.Context) {
		handleBenchChat(c, upstreamURL, channel, benchMetrics)
	})
	router.POST("/v1/responses", func(c *gin.Context) {
		handleBenchResponses(c, upstreamURL, channel, benchMetrics)
	})
	router.GET("/api/metrics", gin.WrapH(promhttp.HandlerFor(benchMetrics.registry, promhttp.HandlerOpts{})))
	return router
}

func handleBenchChat(c *gin.Context, upstreamURL string, channel *model.Channel, benchMetrics *benchMetrics) {
	c.Set("requestStartTime", time.Now())
	c.Set("token_group", "default")
	c.Set("group_ratio", 1.0)
	c.Set("channel_id", channel.Id)
	c.Set("channel_type", channel.Type)
	c.Set(benchTransformModeKey, "native_chat")

	parseStart := time.Now()
	if bodyBytes, err := cacheBenchRequestBody(c); err == nil {
		var meta struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(bodyBytes, &meta) == nil && meta.Model != "" {
			applyBenchPreAdd(c, channel, meta.Model)
		}
	}

	var request types.ChatCompletionRequest
	if err := common.UnmarshalBodyReusable(c, &request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if request.MaxTokens > 0 && request.MaxCompletionTokens == 0 {
		request.MaxCompletionTokens = request.MaxTokens
	}
	request.MaxTokens = 0
	request.NormalizeReasoning()
	if !request.Stream {
		request.StreamOptions = nil
	}
	c.Set(benchRequestParseKey, elapsedMilliseconds(parseStart))

	providerSelectStart := time.Now()
	provider := openai.CreateOpenAIProvider(channel, upstreamURL)
	provider.SetContext(c)
	provider.SetUsage(&types.Usage{})
	originalModel := request.Model
	mappedModel, err := provider.ModelMappingHandler(originalModel)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	request.Model = mappedModel
	c.Set("original_model", originalModel)
	c.Set("new_model", mappedModel)
	c.Set(benchProviderSelectKey, elapsedMilliseconds(providerSelectStart))

	promptCountStart := time.Now()
	_ = common.CountTokenMessages(request.Messages, mappedModel, channel.PreCost)
	c.Set(benchPromptCountKey, elapsedMilliseconds(promptCountStart))

	if request.Stream {
		upstreamHeaderStart := time.Now()
		stream, errWithCode := provider.CreateChatCompletionStream(&request)
		c.Set(benchUpstreamHeaderKey, elapsedMilliseconds(upstreamHeaderStart))
		if errWithCode != nil {
			c.JSON(errWithCode.StatusCode, errWithCode)
			return
		}
		forwardChatStream(c, stream)
	} else {
		response, errWithCode := provider.CreateChatCompletion(&request)
		if errWithCode != nil {
			c.JSON(errWithCode.StatusCode, errWithCode)
			return
		}
		c.JSON(http.StatusOK, response)
	}

	finalizeBenchMetrics(c, benchMetrics)
}

func handleBenchResponses(c *gin.Context, upstreamURL string, channel *model.Channel, benchMetrics *benchMetrics) {
	c.Set("requestStartTime", time.Now())
	c.Set("token_group", "default")
	c.Set("group_ratio", 1.0)
	c.Set("channel_id", channel.Id)
	c.Set("channel_type", channel.Type)
	c.Set(benchTransformModeKey, "native_responses")

	parseStart := time.Now()
	var request types.OpenAIResponsesRequest
	if err := common.UnmarshalBodyReusable(c, &request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.Set(benchRequestParseKey, elapsedMilliseconds(parseStart))

	providerSelectStart := time.Now()
	provider := openai.CreateOpenAIProvider(channel, upstreamURL)
	provider.SetContext(c)
	provider.SetUsage(&types.Usage{})
	originalModel := request.Model
	mappedModel, err := provider.ModelMappingHandler(originalModel)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	request.Model = mappedModel
	c.Set("original_model", originalModel)
	c.Set("new_model", mappedModel)
	c.Set(benchProviderSelectKey, elapsedMilliseconds(providerSelectStart))

	promptCountStart := time.Now()
	_ = common.CountTokenInputMessages(request.Input, mappedModel, channel.PreCost)
	c.Set(benchPromptCountKey, elapsedMilliseconds(promptCountStart))

	if request.Stream {
		upstreamHeaderStart := time.Now()
		stream, errWithCode := provider.CreateResponsesStream(&request)
		c.Set(benchUpstreamHeaderKey, elapsedMilliseconds(upstreamHeaderStart))
		if errWithCode != nil {
			c.JSON(errWithCode.StatusCode, errWithCode)
			return
		}
		forwardResponsesStream(c, stream)
	} else {
		response, errWithCode := provider.CreateResponses(&request)
		if errWithCode != nil {
			c.JSON(errWithCode.StatusCode, errWithCode)
			return
		}
		c.JSON(http.StatusOK, response)
	}

	finalizeBenchMetrics(c, benchMetrics)
}

func applyBenchPreAdd(c *gin.Context, channel *model.Channel, modelName string) {
	customParams, err := getChannelCustomParameterMap(channel)
	if err != nil || customParams == nil {
		return
	}
	preAdd, _ := customParams["pre_add"].(bool)
	if !preAdd {
		return
	}

	requestMap, err := cloneBenchBodyMap(c)
	if err != nil || requestMap == nil {
		return
	}
	modified := baseprovider.ApplyCustomParams(requestMap, customParams, modelName, true)
	modifiedBody, err := json.Marshal(modified)
	if err != nil {
		return
	}
	setBenchRequestBody(c, modifiedBody)
}

func finalizeBenchMetrics(c *gin.Context, benchMetrics *benchMetrics) {
	benchMetrics.observe(c)
}

func forwardChatStream(c *gin.Context, stream requester.StreamReaderInterface[string]) {
	defer stream.Close()
	requester.SetEventStreamHeaders(c)
	dataChan, errChan := stream.Recv()
	first := true

	for {
		select {
		case data, ok := <-dataChan:
			if !ok {
				return
			}
			if first {
				first = false
				setTTFT(c)
			}
			_, _ = c.Writer.Write([]byte("data: " + data + "\n\n"))
			c.Writer.Flush()
		case err := <-errChan:
			if err == nil {
				continue
			}
			if strings.Contains(err.Error(), "EOF") {
				_, _ = c.Writer.Write([]byte("data: [DONE]\n\n"))
				c.Writer.Flush()
				return
			}
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}
	}
}

func forwardResponsesStream(c *gin.Context, stream requester.StreamReaderInterface[string]) {
	defer stream.Close()
	requester.SetEventStreamHeaders(c)
	dataChan, errChan := stream.Recv()
	first := true

	for {
		select {
		case data, ok := <-dataChan:
			if !ok {
				return
			}
			if first {
				first = false
				setTTFT(c)
			}
			_, _ = c.Writer.Write([]byte(data))
			c.Writer.Flush()
		case err := <-errChan:
			if err == nil {
				continue
			}
			if strings.Contains(err.Error(), "EOF") {
				return
			}
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}
	}
}

func setTTFT(c *gin.Context) {
	startTime := c.GetTime("requestStartTime")
	if startTime.IsZero() {
		return
	}
	c.Set(benchTTFTKey, elapsedMilliseconds(startTime))
}

func elapsedMilliseconds(start time.Time) int64 {
	if start.IsZero() {
		return 0
	}
	elapsed := time.Since(start)
	if elapsed <= 0 {
		return 0
	}
	if ms := elapsed.Milliseconds(); ms > 0 {
		return ms
	}
	return 1
}

func cacheBenchRequestBody(c *gin.Context) ([]byte, error) {
	if cached, exists := c.Get(benchCachedBodyKey); exists {
		if requestBody, ok := cached.([]byte); ok {
			c.Request.Body = io.NopCloser(bytes.NewReader(requestBody))
			return requestBody, nil
		}
	}

	requestBody, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return nil, err
	}
	_ = c.Request.Body.Close()
	setBenchRequestBody(c, requestBody)
	return requestBody, nil
}

func setBenchRequestBody(c *gin.Context, requestBody []byte) {
	c.Set(benchCachedBodyKey, requestBody)
	c.Request.Body = io.NopCloser(bytes.NewReader(requestBody))
}

func cloneBenchBodyMap(c *gin.Context) (map[string]interface{}, error) {
	requestBody, err := cacheBenchRequestBody(c)
	if err != nil || len(requestBody) == 0 {
		return nil, err
	}

	requestMap := make(map[string]interface{})
	if err = json.Unmarshal(requestBody, &requestMap); err != nil {
		return nil, err
	}

	clone, ok := cloneBenchJSONValue(requestMap).(map[string]interface{})
	if !ok {
		return nil, nil
	}
	return clone, nil
}

func cloneBenchJSONValue(value interface{}) interface{} {
	switch typed := value.(type) {
	case map[string]interface{}:
		cloned := make(map[string]interface{}, len(typed))
		for key, item := range typed {
			cloned[key] = cloneBenchJSONValue(item)
		}
		return cloned
	case []interface{}:
		cloned := make([]interface{}, len(typed))
		for i, item := range typed {
			cloned[i] = cloneBenchJSONValue(item)
		}
		return cloned
	default:
		return value
	}
}

func getChannelCustomParameterMap(channel *model.Channel) (map[string]interface{}, error) {
	customParameter := channel.GetCustomParameter()
	if customParameter == "" || customParameter == "{}" {
		return nil, nil
	}

	customParams := make(map[string]interface{})
	if err := json.Unmarshal([]byte(customParameter), &customParams); err != nil {
		return nil, err
	}
	return customParams, nil
}

func newMockUpstreamHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", mockChatUpstream)
	mux.HandleFunc("/v1/responses", mockResponsesUpstream)
	return mux
}

func mockChatUpstream(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Stream bool   `json:"stream"`
		Model  string `json:"model"`
	}
	_ = json.NewDecoder(r.Body).Decode(&request)

	time.Sleep(12 * time.Millisecond)
	if request.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		events := []string{
			fmt.Sprintf(`data: {"id":"chatcmpl-bench","object":"chat.completion.chunk","created":%d,"model":"%s","choices":[{"index":0,"delta":{"content":"bench "},"finish_reason":""}]}`+"\n\n", time.Now().Unix(), request.Model),
			fmt.Sprintf(`data: {"id":"chatcmpl-bench","object":"chat.completion.chunk","created":%d,"model":"%s","choices":[{"index":0,"delta":{"content":"response"},"finish_reason":""}]}`+"\n\n", time.Now().Unix(), request.Model),
			fmt.Sprintf(`data: {"id":"chatcmpl-bench","object":"chat.completion.chunk","created":%d,"model":"%s","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":16,"completion_tokens":8,"total_tokens":24}}`+"\n\n", time.Now().Unix(), request.Model),
			"data: [DONE]\n\n",
		}
		for i, event := range events {
			if i > 0 {
				time.Sleep(4 * time.Millisecond)
			}
			_, _ = w.Write([]byte(event))
			if flusher != nil {
				flusher.Flush()
			}
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":      "chatcmpl-bench",
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   request.Model,
		"choices": []map[string]any{
			{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": "bench response",
				},
				"finish_reason": "stop",
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     16,
			"completion_tokens": 8,
			"total_tokens":      24,
		},
	})
}

func mockResponsesUpstream(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Stream bool   `json:"stream"`
		Model  string `json:"model"`
	}
	_ = json.NewDecoder(r.Body).Decode(&request)

	time.Sleep(12 * time.Millisecond)
	if request.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		writer := bufio.NewWriter(w)
		events := []string{
			fmt.Sprintf(`data: {"type":"response.created","response":{"id":"resp_bench","model":"%s","tools":[]}}`+"\n\n", request.Model),
			`data: {"type":"response.output_text.delta","delta":"bench "}` + "\n\n",
			`data: {"type":"response.output_text.delta","delta":"response"}` + "\n\n",
			`data: {"type":"response.completed","response":{"id":"resp_bench","model":"bench","status":"completed","usage":{"input_tokens":16,"output_tokens":8,"total_tokens":24}}}` + "\n\n",
			"data: [DONE]\n\n",
		}
		for i, event := range events {
			if i > 0 {
				time.Sleep(4 * time.Millisecond)
			}
			_, _ = writer.WriteString(event)
			_ = writer.Flush()
			if flusher != nil {
				flusher.Flush()
			}
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":     "resp_bench",
		"object": "response",
		"model":  request.Model,
		"status": "completed",
		"output": []map[string]any{
			{
				"id":     "msg_1",
				"type":   "message",
				"status": "completed",
				"role":   "assistant",
				"content": []map[string]any{
					{
						"type": "output_text",
						"text": "bench response",
					},
				},
			},
		},
		"usage": map[string]any{
			"input_tokens":  16,
			"output_tokens": 8,
			"total_tokens":  24,
		},
	})
}
