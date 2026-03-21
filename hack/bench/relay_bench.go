package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type config struct {
	baseURL         string
	path            string
	model           string
	apiKey          string
	metricsURL      string
	metricsUser     string
	metricsPassword string
	scenario        string
	requestFile     string
	stream          bool
	concurrency     int
	requests        int
	warmup          int
	qps             int
	timeout         time.Duration
	insecure        bool
	selfHosted      bool
}

type sample struct {
	ok            bool
	statusCode    int
	headerLatency time.Duration
	ttft          time.Duration
	total         time.Duration
	responseBytes int64
	err           string
}

type summary struct {
	totalRequests int
	successes     int
	failures      int
	wallTime      time.Duration
	headerP50     time.Duration
	headerP95     time.Duration
	headerP99     time.Duration
	ttftP50       time.Duration
	ttftP95       time.Duration
	ttftP99       time.Duration
	totalP50      time.Duration
	totalP95      time.Duration
	totalP99      time.Duration
	rps           float64
	mbps          float64
	statusCodes   map[int]int
	errors        map[string]int
}

type histogramDelta struct {
	labels  map[string]string
	buckets map[float64]float64
	count   float64
	sum     float64
}

type metricsSnapshot struct {
	histograms map[string]map[string]histogramDelta
	gauges     map[string]float64
}

type histogramReport struct {
	metric string
	labels map[string]string
	count  float64
	avg    float64
	p50    float64
	p95    float64
	p99    float64
}

func main() {
	cfg := parseFlags()
	var localBench *selfHostedBench
	var err error
	if cfg.selfHosted {
		localBench, err = startSelfHostedBench(&cfg)
		if err != nil {
			fatalf("start self-hosted bench failed: %v", err)
		}
		defer localBench.Close()
	}
	if err := validateConfig(&cfg); err != nil {
		fatalf("invalid config: %v", err)
	}

	client := &http.Client{
		Timeout: cfg.timeout,
		Transport: &http.Transport{
			TLSClientConfig:     &tls.Config{InsecureSkipVerify: cfg.insecure}, //nolint:gosec
			ForceAttemptHTTP2:   true,
			MaxIdleConns:        max(64, cfg.concurrency*4),
			MaxIdleConnsPerHost: max(32, cfg.concurrency*2),
			IdleConnTimeout:     90 * time.Second,
		},
	}
	if localBench != nil && localBench.transport != nil {
		client.Transport = localBench.transport
	}

	bodyBytes, err := buildRequestBody(&cfg)
	if err != nil {
		fatalf("build request body failed: %v", err)
	}

	fmt.Printf("bench target: %s%s\n", strings.TrimRight(cfg.baseURL, "/"), cfg.path)
	if cfg.selfHosted {
		fmt.Println("mode: self-hosted")
	}
	fmt.Printf("scenario: %s, stream=%t, requests=%d, concurrency=%d, warmup=%d\n", cfg.scenario, cfg.stream, cfg.requests, cfg.concurrency, cfg.warmup)

	if cfg.warmup > 0 {
		fmt.Printf("warmup: start (%d requests)\n", cfg.warmup)
		for i := 0; i < cfg.warmup; i++ {
			_, err = runOnce(context.Background(), client, cfg, bodyBytes)
			if err != nil {
				fatalf("warmup failed: %v", err)
			}
		}
		fmt.Println("warmup: done")
	}

	beforeMetrics, err := fetchMetricsSnapshot(client, cfg)
	if err != nil {
		fmt.Printf("metrics snapshot before: skipped (%v)\n", err)
	}

	start := time.Now()
	samples, runErr := runLoad(client, cfg, bodyBytes)
	wallTime := time.Since(start)
	if runErr != nil {
		fatalf("load run failed: %v", runErr)
	}

	time.Sleep(100 * time.Millisecond)

	afterMetrics, err := fetchMetricsSnapshot(client, cfg)
	if err != nil {
		fmt.Printf("metrics snapshot after: skipped (%v)\n", err)
	}

	report := summarize(samples, wallTime)
	printClientSummary(report)

	if beforeMetrics != nil && afterMetrics != nil {
		printServerMetrics(diffSnapshots(beforeMetrics, afterMetrics))
	}
}

func parseFlags() config {
	cfg := config{}
	flag.StringVar(&cfg.baseURL, "base-url", envOrDefault("ONE_HUB_BENCH_BASE_URL", "http://127.0.0.1:3000"), "One Hub base URL")
	flag.StringVar(&cfg.path, "path", envOrDefault("ONE_HUB_BENCH_PATH", "/v1/chat/completions"), "request path")
	flag.StringVar(&cfg.model, "model", envOrDefault("ONE_HUB_BENCH_MODEL", "gpt-4o-mini"), "model name")
	flag.StringVar(&cfg.apiKey, "api-key", os.Getenv("ONE_HUB_BENCH_API_KEY"), "bearer token")
	flag.StringVar(&cfg.metricsURL, "metrics-url", os.Getenv("ONE_HUB_BENCH_METRICS_URL"), "metrics URL, defaults to <base-url>/api/metrics")
	flag.StringVar(&cfg.metricsUser, "metrics-user", os.Getenv("ONE_HUB_BENCH_METRICS_USER"), "metrics basic auth user")
	flag.StringVar(&cfg.metricsPassword, "metrics-password", os.Getenv("ONE_HUB_BENCH_METRICS_PASSWORD"), "metrics basic auth password")
	flag.StringVar(&cfg.scenario, "scenario", envOrDefault("ONE_HUB_BENCH_SCENARIO", "short-chat"), "scenario: short-chat,long-chat,tool-heavy,responses-native")
	flag.StringVar(&cfg.requestFile, "request-file", os.Getenv("ONE_HUB_BENCH_REQUEST_FILE"), "custom request JSON file")
	flag.BoolVar(&cfg.stream, "stream", envBool("ONE_HUB_BENCH_STREAM", true), "streaming request")
	flag.IntVar(&cfg.concurrency, "concurrency", envInt("ONE_HUB_BENCH_CONCURRENCY", 16), "concurrent workers")
	flag.IntVar(&cfg.requests, "requests", envInt("ONE_HUB_BENCH_REQUESTS", 200), "total requests")
	flag.IntVar(&cfg.warmup, "warmup", envInt("ONE_HUB_BENCH_WARMUP", 5), "warmup requests")
	flag.IntVar(&cfg.qps, "qps", envInt("ONE_HUB_BENCH_QPS", 0), "overall request rate limit, 0 means unlimited")
	flag.DurationVar(&cfg.timeout, "timeout", envDuration("ONE_HUB_BENCH_TIMEOUT", 2*time.Minute), "per-request timeout")
	flag.BoolVar(&cfg.insecure, "insecure", envBool("ONE_HUB_BENCH_INSECURE", false), "skip TLS verification")
	flag.BoolVar(&cfg.selfHosted, "self-hosted", envBool("ONE_HUB_BENCH_SELF_HOSTED", true), "start a local target and upstream automatically")
	flag.Parse()
	return cfg
}

func validateConfig(cfg *config) error {
	if cfg.baseURL == "" {
		return errors.New("base-url is required")
	}
	if !cfg.selfHosted && cfg.apiKey == "" {
		return errors.New("api-key is required")
	}
	if cfg.concurrency <= 0 {
		return errors.New("concurrency must be greater than 0")
	}
	if cfg.requests <= 0 {
		return errors.New("requests must be greater than 0")
	}
	if cfg.metricsURL == "" {
		cfg.metricsURL = strings.TrimRight(cfg.baseURL, "/") + "/api/metrics"
	}
	return nil
}

func buildRequestBody(cfg *config) ([]byte, error) {
	if cfg.requestFile != "" {
		return os.ReadFile(cfg.requestFile)
	}

	switch cfg.scenario {
	case "short-chat":
		cfg.path = "/v1/chat/completions"
		return mustJSON(map[string]any{
			"model":       cfg.model,
			"stream":      cfg.stream,
			"temperature": 0.2,
			"messages": []map[string]any{
				{"role": "system", "content": "You are a concise assistant."},
				{"role": "user", "content": "Write a short greeting in Chinese and English."},
			},
		}), nil
	case "long-chat":
		cfg.path = "/v1/chat/completions"
		longText := strings.Repeat("请根据以下背景做摘要，并保留关键约束与数据点。", 256)
		return mustJSON(map[string]any{
			"model":       cfg.model,
			"stream":      cfg.stream,
			"temperature": 0.2,
			"messages": []map[string]any{
				{"role": "system", "content": "You are a concise assistant."},
				{"role": "user", "content": longText},
				{"role": "user", "content": "基于以上内容，输出 5 条总结。"},
			},
		}), nil
	case "tool-heavy":
		cfg.path = "/v1/chat/completions"
		return mustJSON(map[string]any{
			"model":       cfg.model,
			"stream":      cfg.stream,
			"temperature": 0.2,
			"messages": []map[string]any{
				{"role": "user", "content": "Plan a business trip and decide whether to call any tool."},
			},
			"tools": buildToolPayload(),
		}), nil
	case "responses-native":
		cfg.path = "/v1/responses"
		return mustJSON(map[string]any{
			"model":  cfg.model,
			"stream": cfg.stream,
			"input": []map[string]any{
				{
					"role": "user",
					"content": []map[string]any{
						{"type": "input_text", "text": "Write a short release note for a performance optimization."},
					},
				},
			},
		}), nil
	default:
		return nil, fmt.Errorf("unsupported scenario: %s", cfg.scenario)
	}
}

func buildToolPayload() []map[string]any {
	tools := make([]map[string]any, 0, 8)
	for i := 0; i < 8; i++ {
		tools = append(tools, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        fmt.Sprintf("tool_%d", i+1),
				"description": fmt.Sprintf("Synthetic bench tool %d", i+1),
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"city": map[string]any{"type": "string"},
						"date": map[string]any{"type": "string"},
					},
					"required": []string{"city"},
				},
			},
		})
	}
	return tools
}

func runLoad(client *http.Client, cfg config, body []byte) ([]sample, error) {
	samples := make([]sample, cfg.requests)
	jobs := make(chan int)

	var wg sync.WaitGroup
	var limiter <-chan time.Time
	if cfg.qps > 0 {
		ticker := time.NewTicker(time.Second / time.Duration(cfg.qps))
		defer ticker.Stop()
		limiter = ticker.C
	}

	var failed atomic.Bool
	for worker := 0; worker < cfg.concurrency; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				if limiter != nil {
					<-limiter
				}
				s, err := runOnce(context.Background(), client, cfg, body)
				if err != nil {
					failed.Store(true)
					s = sample{err: err.Error()}
				}
				samples[idx] = s
			}
		}()
	}

	for i := 0; i < cfg.requests; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	if failed.Load() {
		return samples, nil
	}
	return samples, nil
}

func runOnce(ctx context.Context, client *http.Client, cfg config, body []byte) (sample, error) {
	url := strings.TrimRight(cfg.baseURL, "/") + cfg.path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return sample{}, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if cfg.stream {
		req.Header.Set("Accept", "text/event-stream")
	}

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return sample{}, err
	}
	defer resp.Body.Close()

	result := sample{
		statusCode:    resp.StatusCode,
		headerLatency: time.Since(start),
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		result.err = strings.TrimSpace(string(bodyBytes))
		result.total = time.Since(start)
		return result, nil
	}

	if cfg.stream {
		result, err = readStreamResponse(resp.Body, start, result)
		if err != nil {
			result.err = err.Error()
		}
	} else {
		n, err := io.Copy(io.Discard, resp.Body)
		result.responseBytes = n
		result.total = time.Since(start)
		result.ttft = result.headerLatency
		if err != nil {
			result.err = err.Error()
		}
	}

	result.ok = result.err == ""
	return result, nil
}

func readStreamResponse(body io.Reader, start time.Time, base sample) (sample, error) {
	reader := bufio.NewReader(body)
	result := base
	var firstTokenSeen bool

	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			result.responseBytes += int64(len(line))
			trimmed := strings.TrimSpace(string(line))
			if !firstTokenSeen && strings.HasPrefix(trimmed, "data:") {
				payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
				if payload != "" && payload != "[DONE]" {
					firstTokenSeen = true
					result.ttft = time.Since(start)
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			result.total = time.Since(start)
			return result, err
		}
	}

	result.total = time.Since(start)
	if result.ttft == 0 {
		result.ttft = result.headerLatency
	}
	return result, nil
}

func summarize(samples []sample, wallTime time.Duration) summary {
	report := summary{
		totalRequests: len(samples),
		wallTime:      wallTime,
		statusCodes:   make(map[int]int),
		errors:        make(map[string]int),
	}

	headerDurations := make([]time.Duration, 0, len(samples))
	ttftDurations := make([]time.Duration, 0, len(samples))
	totalDurations := make([]time.Duration, 0, len(samples))
	var totalBytes int64

	for _, item := range samples {
		if item.statusCode != 0 {
			report.statusCodes[item.statusCode]++
		}
		if item.ok {
			report.successes++
			headerDurations = append(headerDurations, item.headerLatency)
			ttftDurations = append(ttftDurations, item.ttft)
			totalDurations = append(totalDurations, item.total)
			totalBytes += item.responseBytes
			continue
		}
		report.failures++
		if item.err != "" {
			report.errors[item.err]++
		}
	}

	report.headerP50 = percentileDuration(headerDurations, 0.50)
	report.headerP95 = percentileDuration(headerDurations, 0.95)
	report.headerP99 = percentileDuration(headerDurations, 0.99)
	report.ttftP50 = percentileDuration(ttftDurations, 0.50)
	report.ttftP95 = percentileDuration(ttftDurations, 0.95)
	report.ttftP99 = percentileDuration(ttftDurations, 0.99)
	report.totalP50 = percentileDuration(totalDurations, 0.50)
	report.totalP95 = percentileDuration(totalDurations, 0.95)
	report.totalP99 = percentileDuration(totalDurations, 0.99)

	if wallTime > 0 {
		report.rps = float64(report.successes) / wallTime.Seconds()
		report.mbps = float64(totalBytes) / wallTime.Seconds() / (1024 * 1024)
	}
	return report
}

func printClientSummary(report summary) {
	fmt.Println()
	fmt.Println("== Client Summary ==")
	fmt.Printf("requests: total=%d success=%d fail=%d wall=%s rps=%.2f resp_mib_per_s=%.2f\n",
		report.totalRequests, report.successes, report.failures, report.wallTime.Round(time.Millisecond), report.rps, report.mbps)
	fmt.Printf("header latency: p50=%s p95=%s p99=%s\n", formatDuration(report.headerP50), formatDuration(report.headerP95), formatDuration(report.headerP99))
	fmt.Printf("client ttft:    p50=%s p95=%s p99=%s\n", formatDuration(report.ttftP50), formatDuration(report.ttftP95), formatDuration(report.ttftP99))
	fmt.Printf("total latency:  p50=%s p95=%s p99=%s\n", formatDuration(report.totalP50), formatDuration(report.totalP95), formatDuration(report.totalP99))

	if len(report.statusCodes) > 0 {
		fmt.Println("status codes:")
		for _, code := range sortedIntKeys(report.statusCodes) {
			fmt.Printf("  %d -> %d\n", code, report.statusCodes[code])
		}
	}

	if len(report.errors) > 0 {
		fmt.Println("top errors:")
		for _, item := range topErrors(report.errors, 5) {
			fmt.Printf("  %dx %s\n", item.count, item.err)
		}
	}
}

func fetchMetricsSnapshot(client *http.Client, cfg config) (*metricsSnapshot, error) {
	metricsURL := cfg.metricsURL
	if metricsURL == "" {
		metricsURL = strings.TrimRight(cfg.baseURL, "/") + "/api/metrics"
	}

	req, err := http.NewRequest(http.MethodGet, metricsURL, nil)
	if err != nil {
		return nil, err
	}
	if cfg.metricsUser != "" || cfg.metricsPassword != "" {
		req.SetBasicAuth(cfg.metricsUser, cfg.metricsPassword)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("metrics status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return parseMetricsSnapshot(body)
}

func diffSnapshots(before, after *metricsSnapshot) []histogramReport {
	if before == nil || after == nil {
		return nil
	}

	targets := []string{
		"request_parse_ms",
		"provider_select_ms",
		"prompt_count_ms",
		"upstream_header_ms",
		"ttft_ms",
	}

	var reports []histogramReport
	for _, metric := range targets {
		beforeByKey := before.histograms[metric]
		afterByKey := after.histograms[metric]
		for key, afterHist := range afterByKey {
			beforeHist := beforeByKey[key]
			delta := histogramDelta{
				labels:  cloneLabels(afterHist.labels),
				buckets: make(map[float64]float64, len(afterHist.buckets)),
				count:   afterHist.count - beforeHist.count,
				sum:     afterHist.sum - beforeHist.sum,
			}
			if delta.count <= 0 {
				continue
			}
			for le, count := range afterHist.buckets {
				delta.buckets[le] = count - beforeHist.buckets[le]
			}
			reports = append(reports, histogramReport{
				metric: metric,
				labels: delta.labels,
				count:  delta.count,
				avg:    safeDivide(delta.sum, delta.count),
				p50:    histogramQuantile(delta.buckets, delta.count, 0.50),
				p95:    histogramQuantile(delta.buckets, delta.count, 0.95),
				p99:    histogramQuantile(delta.buckets, delta.count, 0.99),
			})
		}
	}

	sort.Slice(reports, func(i, j int) bool {
		if reports[i].metric != reports[j].metric {
			return reports[i].metric < reports[j].metric
		}
		pi := reports[i].labels["path"]
		pj := reports[j].labels["path"]
		if pi != pj {
			return pi < pj
		}
		return reports[i].labels["transform_mode"] < reports[j].labels["transform_mode"]
	})
	return reports
}

func printServerMetrics(reports []histogramReport) {
	if len(reports) == 0 {
		fmt.Println()
		fmt.Println("== Server Metrics Delta ==")
		fmt.Println("no histogram delta collected")
		return
	}

	fmt.Println()
	fmt.Println("== Server Metrics Delta ==")
	for _, report := range reports {
		fmt.Printf("%s path=%s transform_mode=%s count=%.0f avg=%.2fms p50<=%.2fms p95<=%.2fms p99<=%.2fms\n",
			report.metric,
			report.labels["path"],
			report.labels["transform_mode"],
			report.count,
			report.avg,
			report.p50,
			report.p95,
			report.p99,
		)
	}
}

func parseMetricsSnapshot(raw []byte) (*metricsSnapshot, error) {
	snapshot := &metricsSnapshot{
		histograms: make(map[string]map[string]histogramDelta),
		gauges:     make(map[string]float64),
	}

	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		name, labels, value, err := parseMetricLine(line)
		if err != nil {
			continue
		}

		switch {
		case strings.HasSuffix(name, "_bucket"):
			baseName := strings.TrimSuffix(name, "_bucket")
			le, err := strconv.ParseFloat(labels["le"], 64)
			if err != nil {
				continue
			}
			delete(labels, "le")
			key := labelsKey(labels)
			if snapshot.histograms[baseName] == nil {
				snapshot.histograms[baseName] = make(map[string]histogramDelta)
			}
			entry := snapshot.histograms[baseName][key]
			entry.labels = cloneLabels(labels)
			if entry.buckets == nil {
				entry.buckets = make(map[float64]float64)
			}
			entry.buckets[le] = value
			snapshot.histograms[baseName][key] = entry
		case strings.HasSuffix(name, "_sum"):
			baseName := strings.TrimSuffix(name, "_sum")
			key := labelsKey(labels)
			if snapshot.histograms[baseName] == nil {
				snapshot.histograms[baseName] = make(map[string]histogramDelta)
			}
			entry := snapshot.histograms[baseName][key]
			entry.labels = cloneLabels(labels)
			entry.sum = value
			snapshot.histograms[baseName][key] = entry
		case strings.HasSuffix(name, "_count"):
			baseName := strings.TrimSuffix(name, "_count")
			key := labelsKey(labels)
			if snapshot.histograms[baseName] == nil {
				snapshot.histograms[baseName] = make(map[string]histogramDelta)
			}
			entry := snapshot.histograms[baseName][key]
			entry.labels = cloneLabels(labels)
			entry.count = value
			snapshot.histograms[baseName][key] = entry
		default:
			snapshot.gauges[name] = value
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func parseMetricLine(line string) (string, map[string]string, float64, error) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return "", nil, 0, fmt.Errorf("invalid metric line: %s", line)
	}

	nameAndLabels := fields[0]
	value, err := strconv.ParseFloat(fields[len(fields)-1], 64)
	if err != nil {
		return "", nil, 0, err
	}

	name := nameAndLabels
	labels := make(map[string]string)
	if idx := strings.IndexByte(nameAndLabels, '{'); idx >= 0 {
		name = nameAndLabels[:idx]
		rawLabels := strings.TrimSuffix(nameAndLabels[idx+1:], "}")
		labels, err = parseLabelSet(rawLabels)
		if err != nil {
			return "", nil, 0, err
		}
	}

	return name, labels, value, nil
}

func parseLabelSet(raw string) (map[string]string, error) {
	if raw == "" {
		return map[string]string{}, nil
	}

	labels := make(map[string]string)
	for len(raw) > 0 {
		eq := strings.IndexByte(raw, '=')
		if eq <= 0 {
			return nil, fmt.Errorf("invalid label set: %s", raw)
		}
		key := raw[:eq]
		raw = raw[eq+1:]
		if !strings.HasPrefix(raw, "\"") {
			return nil, fmt.Errorf("invalid label value: %s", raw)
		}
		raw = raw[1:]

		var buf strings.Builder
		escaped := false
		i := 0
		for ; i < len(raw); i++ {
			ch := raw[i]
			if escaped {
				buf.WriteByte(ch)
				escaped = false
				continue
			}
			if ch == '\\' {
				escaped = true
				continue
			}
			if ch == '"' {
				break
			}
			buf.WriteByte(ch)
		}
		if i >= len(raw) {
			return nil, fmt.Errorf("unterminated label value: %s", raw)
		}
		labels[key] = buf.String()
		raw = raw[i+1:]
		if strings.HasPrefix(raw, ",") {
			raw = raw[1:]
		}
	}

	return labels, nil
}

func percentileDuration(values []time.Duration, p float64) time.Duration {
	if len(values) == 0 {
		return 0
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	index := int(math.Ceil(float64(len(values))*p)) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(values) {
		index = len(values) - 1
	}
	return values[index]
}

func histogramQuantile(buckets map[float64]float64, total float64, q float64) float64 {
	if len(buckets) == 0 || total <= 0 {
		return 0
	}
	target := total * q
	les := make([]float64, 0, len(buckets))
	for le := range buckets {
		les = append(les, le)
	}
	sort.Float64s(les)
	for _, le := range les {
		if buckets[le] >= target {
			return le
		}
	}
	return les[len(les)-1]
}

func labelsKey(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var buf strings.Builder
	for i, key := range keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.WriteString(key)
		buf.WriteByte('=')
		buf.WriteString(labels[key])
	}
	return buf.String()
}

func cloneLabels(labels map[string]string) map[string]string {
	if len(labels) == 0 {
		return map[string]string{}
	}
	cloned := make(map[string]string, len(labels))
	for key, value := range labels {
		cloned[key] = value
	}
	return cloned
}

func mustJSON(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return data
}

func safeDivide(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return a / b
}

func formatDuration(d time.Duration) string {
	if d == 0 {
		return "-"
	}
	return d.Round(time.Millisecond).String()
}

type errorStat struct {
	err   string
	count int
}

func topErrors(errorsMap map[string]int, n int) []errorStat {
	stats := make([]errorStat, 0, len(errorsMap))
	for errText, count := range errorsMap {
		stats = append(stats, errorStat{err: errText, count: count})
	}
	sort.Slice(stats, func(i, j int) bool {
		if stats[i].count != stats[j].count {
			return stats[i].count > stats[j].count
		}
		return stats[i].err < stats[j].err
	})
	if len(stats) > n {
		stats = stats[:n]
	}
	return stats
}

func sortedIntKeys(m map[int]int) []int {
	keys := make([]int, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Ints(keys)
	return keys
}

func envOrDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func envInt(key string, fallback int) int {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envBool(key string, fallback bool) bool {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envDuration(key string, fallback time.Duration) time.Duration {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
