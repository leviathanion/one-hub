package antigravity

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"one-api/common/requester"
	commonresponses "one-api/common/responses"
	"one-api/providers/antigravity/wire"
	"one-api/providers/base"
	"one-api/providers/claude"
	"one-api/providers/gemini"
	"one-api/types"
)

const protocolUsageJSON = `{"promptTokenCount":7,"candidatesTokenCount":2,"thoughtsTokenCount":3,"totalTokenCount":12}`

func TestProtocolResponsesParametersCannotRewriteLifecycle(t *testing.T) {
	for _, tc := range []struct{ name, input, parameters, field string }{
		{"remove continuation", `{"model":"mapped","store":false,"input":"hi","previous_response_id":"resp_existing"}`, `{"pre_add":true,"remove_params":["previous_response_id"]}`, "previous_response_id"},
		{"remove conversation", `{"model":"mapped","store":false,"input":"hi","conversation":"conv_existing"}`, `{"pre_add":true,"remove_params":["conversation"]}`, "conversation"},
		{"change storage", `{"model":"mapped","store":false,"input":"hi"}`, `{"pre_add":true,"overwrite":true,"store":true}`, "store"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := protocolProvider(t, "http://unused.invalid", tc.input)
			p.Channel.Key = "invalid credentials must never be read"
			p.Channel.CustomParameter = &tc.parameters
			var fields map[string]json.RawMessage
			if err := json.Unmarshal([]byte(tc.input), &fields); err != nil {
				t.Fatal(err)
			}
			err := (AntigravityProviderFactory{}).ValidateResponsesRequest(p.Channel, base.OperationResponsesCreate, fields, "mapped", true)
			var capability *base.RequestCapabilityError
			if !errors.As(err, &capability) || capability.Param != tc.field {
				t.Fatalf("assessment error=%v", err)
			}
			for _, streaming := range []bool{false, true} {
				_, apiErr, streamErr := callProtocol(t, p, wire.DialectResponses, streaming, tc.input)
				if apiErr == nil || apiErr.StatusCode != 400 || apiErr.Param != tc.field || !apiErr.UpstreamNotAttempted || streamErr != nil {
					t.Fatalf("lifecycle was changed before dispatch: apiErr=%v streamErr=%v", apiErr, streamErr)
				}
			}
		})
	}
}

func protocolInput(dialect wire.Dialect) string {
	switch dialect {
	case wire.DialectGemini:
		return `{"contents":[{"role":"user","parts":[{"text":"hello user"}]}],"futureRequest":{"n":900719925474099312345,"union":[null,"future"]}}`
	case wire.DialectChat:
		return `{"model":"client-alias","messages":[{"role":"user","content":"hello user"}],"tools":[{"type":"function","function":{"name":"inspect","parameters":{"type":"object","futureRequest":{"n":900719925474099312345,"union":[null,"future"]}}}}]}`
	case wire.DialectClaude:
		return `{"model":"client-alias","max_tokens":128,"messages":[{"role":"user","content":"hello user"}],"tools":[{"name":"inspect","input_schema":{"type":"object","futureRequest":{"n":900719925474099312345,"union":[null,"future"]}}}]}`
	default:
		return `{"model":"client-alias","store":false,"input":"hello user","tools":[{"type":"function","name":"inspect","parameters":{"type":"object","futureRequest":{"n":900719925474099312345,"union":[null,"future"]}}}]}`
	}
}

func protocolProvider(t *testing.T, endpoint, input string) *AntigravityProvider {
	t.Helper()
	previousMode := gin.Mode()
	gin.SetMode(gin.TestMode)
	t.Cleanup(func() { gin.SetMode(previousMode) })
	p := modelDiscoveryProvider(t, endpoint)
	p.Requester.ErrorHandler = p.handleHTTPError
	p.Usage = &types.Usage{}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/client-entry", strings.NewReader(input)).WithContext(t.Context())
	c.Request.Header.Set("Authorization", "Bearer one-hub-client-secret")
	c.Request.Header.Set("Content-Type", "application/json")
	p.SetContext(c)
	return p
}

func TestProtocolNativeErrorsWithoutMessagePreserveWireAndRedact(t *testing.T) {
	const upstreamError = `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","details":[{"@type":"future.diagnostic","description":"model-discovery-subject model-discovery-project model-discovery-access model-discovery-refresh","retryDelay":"2s"}],"futureUnion":[null,{"n":900719925474099312345}]},"futureTop":true}`
	for _, streaming := range []bool{false, true} {
		t.Run(strconv.FormatBool(streaming), func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				if streaming {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: "+upstreamError+"\n\n")
				} else {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusTooManyRequests)
					_, _ = io.WriteString(w, upstreamError)
				}
			}))
			defer server.Close()
			input := protocolInput(wire.DialectGemini)
			_, apiErr, streamErr := callProtocol(t, protocolProvider(t, server.URL, input), wire.DialectGemini, streaming, input)
			if streaming && !errors.As(streamErr, &apiErr) {
				t.Fatalf("SSE error type lost: %v", streamErr)
			}
			if apiErr == nil || apiErr.StatusCode != 429 || !apiErr.ReplayRawResponse || requests.Load() != 1 {
				t.Fatalf("native error=%v requests=%d", apiErr, requests.Load())
			}
			raw := string(apiErr.RawBody)
			for _, expected := range []string{`"code":429`, `"status":"RESOURCE_EXHAUSTED"`, `"retryDelay":"2s"`, `"futureUnion":[null,{"n":900719925474099312345}]`, `"futureTop":true`} {
				if !strings.Contains(raw, expected) {
					t.Fatalf("native error lost %s: %s", expected, raw)
				}
			}
			if strings.Contains(raw, `"message"`) {
				t.Fatalf("synthetic message altered native error: %s", raw)
			}
			for _, secret := range []string{"model-discovery-subject", "model-discovery-project", "model-discovery-access", "model-discovery-refresh"} {
				if strings.Contains(raw, secret) || strings.Contains(apiErr.Error(), secret) {
					t.Fatalf("native error leaked %q", secret)
				}
			}
		})
	}
}

func TestProtocolChatToolSignatureReturnsToUpstreamOnNextTurn(t *testing.T) {
	var requests atomic.Int32
	captured := make(chan []byte, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		turn := requests.Add(1)
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		captured <- raw
		w.Header().Set("Content-Type", "text/event-stream")
		if turn == 1 {
			_, _ = io.WriteString(w, "data: {\"response\":{\"candidates\":[{\"index\":0,\"content\":{\"parts\":[{\"functionCall\":{\"id\":\"call-native\",\"name\":\"inspect\",\"args\":{\"n\":1}},\"thoughtSignature\":\"original-native-tool-signature\"}]},\"finishReason\":\"STOP\"}]}}\n\n")
		} else {
			_, _ = io.WriteString(w, "data: {\"response\":{\"candidates\":[{\"index\":0,\"content\":{\"parts\":[{\"text\":\"done\"}]},\"finishReason\":\"STOP\"}]}}\n\n")
		}
	}))
	defer server.Close()
	first := protocolInput(wire.DialectChat)
	output, apiErr, streamErr := callProtocol(t, protocolProvider(t, server.URL, first), wire.DialectChat, false, first)
	if apiErr != nil || streamErr != nil {
		t.Fatalf("first turn failed: %v %v", apiErr, streamErr)
	}
	<-captured
	var response struct {
		Choices []struct {
			Message json.RawMessage `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(output, &response); err != nil || len(response.Choices) != 1 {
		t.Fatalf("invalid first output: %s %v", output, err)
	}
	message := response.Choices[0].Message
	if !bytes.Contains(message, []byte(`"antigravity_state":"ag1.`)) {
		t.Fatalf("tool signature carrier missing: %s", message)
	}
	var assistant struct {
		ToolCalls []struct {
			ID string `json:"id"`
		} `json:"tool_calls"`
	}
	if err := json.Unmarshal(message, &assistant); err != nil || len(assistant.ToolCalls) != 1 {
		t.Fatalf("tool identity missing: %s %v", message, err)
	}
	toolResult, _ := json.Marshal(map[string]string{"role": "tool", "tool_call_id": assistant.ToolCalls[0].ID, "content": "observed"})
	second := `{"model":"client-alias","messages":[{"role":"user","content":"hello user"},` + string(message) + `,` + string(toolResult) + `]}`
	output, apiErr, streamErr = callProtocol(t, protocolProvider(t, server.URL, second), wire.DialectChat, false, second)
	if apiErr != nil || streamErr != nil || !bytes.Contains(output, []byte("done")) {
		t.Fatalf("second turn failed: %v %v %s", apiErr, streamErr, output)
	}
	upstream := <-captured
	if !bytes.Contains(upstream, []byte(`"thoughtSignature":"original-native-tool-signature"`)) || bytes.Contains(upstream, []byte("antigravity_state")) || requests.Load() != 2 {
		t.Fatalf("original tool signature did not round trip: %s requests=%d", upstream, requests.Load())
	}
}

func TestProtocolNativeImageGenerationUsesExplicitRequestType(t *testing.T) {
	for _, tc := range []struct{ modality, requestType string }{{"IMAGE", "image_gen"}, {"TEXT", "agent"}} {
		t.Run(tc.modality, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Model       string          `json:"model"`
					RequestType string          `json:"requestType"`
					RequestID   string          `json:"requestId"`
					Request     json.RawMessage `json:"request"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				if body.Model != "upstream-model" || body.RequestType != tc.requestType || !strings.HasPrefix(body.RequestID, tc.requestType) {
					t.Errorf("incorrect envelope: %+v", body)
				}
				if !bytes.Contains(body.Request, []byte(`"maxOutputTokens":-7`)) {
					t.Errorf("owned envelope rewrite changed upstream parameter: %s", body.Request)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"response":{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}]}}`)
			}))
			defer server.Close()
			input := `{"contents":{"futureUnion":[1,null]},"generationConfig":{"responseModalities":["` + tc.modality + `"],"maxOutputTokens":-7}}`
			_, apiErr, streamErr := callProtocol(t, protocolProvider(t, server.URL, input), wire.DialectGemini, false, input)
			if apiErr != nil || streamErr != nil {
				t.Fatalf("native future request failed: %v %v", apiErr, streamErr)
			}
		})
	}
}

func readProtocolStream(t *testing.T, stream requester.StreamReaderInterface[string]) ([]byte, error) {
	t.Helper()
	defer requester.CloseAndDrainStream(stream)
	data, failures := stream.Recv()
	var output bytes.Buffer
	var failure error
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for data != nil || failures != nil {
		select {
		case <-timer.C:
			t.Fatal("provider stream did not terminate")
		case frame, ok := <-data:
			if !ok {
				data = nil
			} else {
				output.WriteString(frame)
			}
		case err, ok := <-failures:
			if !ok {
				failures = nil
			} else if err != nil && !errors.Is(err, io.EOF) {
				failure = errors.Join(failure, err)
			}
		}
	}
	return output.Bytes(), failure
}

func callProtocol(t *testing.T, p *AntigravityProvider, dialect wire.Dialect, streaming bool, input string) ([]byte, *types.OpenAIErrorWithStatusCode, error) {
	t.Helper()
	var stream requester.StreamReaderInterface[string]
	var apiErr *types.OpenAIErrorWithStatusCode
	switch dialect {
	case wire.DialectGemini:
		req := &gemini.NativeRequest{Raw: []byte(input), Model: "upstream-model", Stream: streaming}
		if streaming {
			stream, apiErr = p.CreateGeminiChatStream(req)
		} else {
			result, failure := p.CreateGeminiChat(req)
			if failure != nil {
				return nil, failure, nil
			}
			return result.ReplayProviderRawJSON(), nil, nil
		}
	case wire.DialectClaude:
		req := &claude.ClaudeRequest{}
		if err := json.Unmarshal([]byte(input), req); err != nil {
			t.Fatal(err)
		}
		req.Model = "upstream-model"
		if streaming {
			stream, apiErr = p.CreateClaudeChatStream(req)
		} else {
			result, failure := p.CreateClaudeChat(req)
			if failure != nil {
				return nil, failure, nil
			}
			return result.ReplayProviderRawJSON(), nil, nil
		}
	case wire.DialectChat:
		req := &types.ChatCompletionRequest{}
		if err := json.Unmarshal([]byte(input), req); err != nil {
			t.Fatal(err)
		}
		req.Model = "upstream-model"
		if streaming {
			stream, apiErr = p.CreateChatCompletionStream(req)
		} else {
			result, failure := p.CreateChatCompletion(req)
			if failure != nil {
				return nil, failure, nil
			}
			return result.ReplayProviderRawJSON(), nil, nil
		}
	case wire.DialectResponses:
		body, err := commonresponses.ParseRawEnvelope([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		req := &commonresponses.Request{Operation: commonresponses.ResponsesCreate, Body: body, Model: "upstream-model", Control: commonresponses.Control{Stream: streaming}}
		if streaming {
			stream, apiErr = p.CreateResponsesStream(t.Context(), req)
		} else {
			result, failure := p.CreateResponses(t.Context(), req)
			if failure != nil {
				return nil, failure, nil
			}
			return result.ReplayProviderRawJSON(), nil, nil
		}
	default:
		t.Fatal("unknown test protocol")
	}
	if apiErr != nil {
		return nil, apiErr, nil
	}
	if stream == nil {
		t.Fatal("provider returned nil stream without error")
	}
	output, err := readProtocolStream(t, stream)
	return output, nil, err
}

func TestProtocolFourEntrypointsUseExpectedWireAndUsage(t *testing.T) {
	for _, dialect := range []wire.Dialect{wire.DialectGemini, wire.DialectClaude, wire.DialectChat, wire.DialectResponses} {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", dialect, streaming), func(t *testing.T) {
				var requests atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					wantPath, wantQuery := "/v1internal:streamGenerateContent", "alt=sse"
					if dialect == wire.DialectGemini && !streaming {
						wantPath, wantQuery = "/v1internal:generateContent", ""
					}
					if r.Method != http.MethodPost || r.URL.Path != wantPath || r.URL.RawQuery != wantQuery {
						t.Errorf("unexpected upstream URL: %s %s", r.Method, r.URL)
					}
					if r.Header.Get("Authorization") != "Bearer model-discovery-access" {
						t.Error("upstream did not use the OAuth bearer")
					}
					raw, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
						return
					}
					if strings.Contains(string(raw), "one-hub-client-secret") || strings.Contains(fmt.Sprint(r.Header), "one-hub-client-secret") {
						t.Error("client bearer leaked upstream")
					}
					var body struct {
						Project     string          `json:"project"`
						Model       string          `json:"model"`
						RequestType string          `json:"requestType"`
						Request     json.RawMessage `json:"request"`
					}
					if err := json.Unmarshal(raw, &body); err != nil {
						t.Error(err)
						return
					}
					if body.Project != "model-discovery-project" || body.Model != "upstream-model" || body.RequestType != "agent" {
						t.Errorf("wrong control envelope: %s", raw)
					}
					if !bytes.Contains(body.Request, []byte(`900719925474099312345`)) || !bytes.Contains(body.Request, []byte(`"union":[null,"future"]`)) {
						t.Errorf("raw future data changed: %s", body.Request)
					}
					inner := `{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"hello answer"}]},"finishReason":"STOP"}],"usageMetadata":` + protocolUsageJSON
					if dialect == wire.DialectGemini {
						inner += `,"futureResponse":{"n":900719925474099312345}`
					}
					inner += `}`
					if dialect == wire.DialectGemini && !streaming {
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, `{"project":"model-discovery-project","response":`+inner+`}`)
					} else {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, "data: {\"response\":"+inner+"}\n\n")
					}
				}))
				defer server.Close()
				input := protocolInput(dialect)
				p := protocolProvider(t, server.URL, input)
				output, apiErr, streamErr := callProtocol(t, p, dialect, streaming, input)
				if apiErr != nil || streamErr != nil || requests.Load() != 1 {
					t.Fatalf("apiErr=%v streamErr=%v requests=%d output=%s", apiErr, streamErr, requests.Load(), output)
				}
				if !bytes.Contains(output, []byte("hello answer")) || bytes.Contains(output, []byte("model-discovery-project")) || bytes.Contains(output, []byte("model-discovery-access")) {
					t.Fatalf("invalid public output: %s", output)
				}
				if dialect == wire.DialectGemini && !bytes.Contains(output, []byte(`"futureResponse":{"n":900719925474099312345}`)) {
					t.Fatalf("native response lost extension: %s", output)
				}
				if p.Usage == nil || !p.Usage.ProviderReported || p.Usage.PromptTokens != 7 || p.Usage.CompletionTokens != 5 || p.Usage.TotalTokens != 12 {
					t.Fatalf("usage=%+v", p.Usage)
				}
				if streaming && dialect != wire.DialectGemini {
					terminal := map[wire.Dialect]string{wire.DialectChat: "data: [DONE]", wire.DialectClaude: "event: message_stop", wire.DialectResponses: "event: response.completed"}[dialect]
					if strings.Count(string(output), terminal) != 1 {
						t.Fatalf("missing or duplicate terminal %q: %s", terminal, output)
					}
				}
			})
		}
	}
}

func TestProtocolHTTPFailuresPreserveStatusAndRedactCredentials(t *testing.T) {
	for _, dialect := range []wire.Dialect{wire.DialectGemini, wire.DialectClaude, wire.DialectChat, wire.DialectResponses} {
		for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusServiceUnavailable} {
			t.Run(fmt.Sprintf("%s/%d", dialect, status), func(t *testing.T) {
				var requests atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					requests.Add(1)
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(status)
					_, _ = fmt.Fprintf(w, `{"error":{"code":%d,"status":"RESOURCE_EXHAUSTED","message":"secret model-discovery-access model-discovery-refresh model-discovery-project model-discovery-subject"}}`, status)
				}))
				defer server.Close()
				input := protocolInput(dialect)
				output, apiErr, streamErr := callProtocol(t, protocolProvider(t, server.URL, input), dialect, true, input)
				if apiErr == nil || apiErr.StatusCode != status || streamErr != nil || len(output) != 0 || requests.Load() != 1 {
					t.Fatalf("apiErr=%v streamErr=%v requests=%d", apiErr, streamErr, requests.Load())
				}
				for _, secret := range []string{"model-discovery-access", "model-discovery-refresh", "model-discovery-project", "model-discovery-subject"} {
					if strings.Contains(apiErr.Error(), secret) || bytes.Contains(apiErr.RawBody, []byte(secret)) {
						t.Fatalf("upstream secret %q escaped redaction", secret)
					}
				}
			})
		}
	}
}

func TestProtocolResponsesStoreDefaultRefusedBeforeHTTP(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(strconv.FormatBool(streaming), func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { requests.Add(1); w.WriteHeader(500) }))
			defer server.Close()
			input := `{"model":"client-alias","input":"hello"}`
			_, apiErr, _ := callProtocol(t, protocolProvider(t, server.URL, input), wire.DialectResponses, streaming, input)
			if apiErr == nil || apiErr.StatusCode != 400 || apiErr.Param != "store" || !apiErr.UpstreamNotAttempted || requests.Load() != 0 {
				t.Fatalf("apiErr=%v requests=%d", apiErr, requests.Load())
			}
		})
	}
}

func TestProtocolIncompleteAndFailedStreamsNeverReplayOrSynthesizeSuccess(t *testing.T) {
	for _, dialect := range []wire.Dialect{wire.DialectGemini, wire.DialectClaude, wire.DialectChat, wire.DialectResponses} {
		for _, streaming := range []bool{false, true} {
			if dialect == wire.DialectGemini && !streaming {
				continue
			}
			modes := []string{"missing terminal", "tail error", "transport truncation"}
			if dialect != wire.DialectGemini {
				modes = append(modes, "unrepresentable response")
			}
			for _, mode := range modes {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", dialect, streaming, mode), func(t *testing.T) {
					var requests atomic.Int32
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						requests.Add(1)
						body := `data: {"response":{"candidates":[{"index":0,"content":{"parts":[{"text":"partial"}]}}],"usageMetadata":` + protocolUsageJSON + "}}\n\n"
						if mode == "tail error" {
							body += "data: {\"response\":{\"candidates\":[{\"index\":0,\"finishReason\":\"STOP\"}]}}\n\n"
							body += "data: {\"error\":{\"code\":503,\"status\":\"UNAVAILABLE\",\"message\":\"late failure\"}}\n\n"
						}
						if mode == "unrepresentable response" {
							body += "data: {\"response\":{\"futureResponse\":{\"meaning\":\"cannot drop\"},\"candidates\":[{\"index\":0,\"finishReason\":\"STOP\"}]}}\n\n"
						}
						w.Header().Set("Content-Type", "text/event-stream")
						if mode == "transport truncation" {
							w.Header().Set("Content-Length", strconv.Itoa(len(body)+100))
						}
						_, _ = io.WriteString(w, body)
					}))
					defer server.Close()
					input := protocolInput(dialect)
					p := protocolProvider(t, server.URL, input)
					output, apiErr, streamErr := callProtocol(t, p, dialect, streaming, input)
					if apiErr == nil && streamErr == nil {
						t.Fatalf("failed upstream became success: %s", output)
					}
					if requests.Load() != 1 {
						t.Fatalf("failed request replayed %d times", requests.Load())
					}
					for _, terminal := range []string{"data: [DONE]", "event: message_stop", "event: response.completed"} {
						if bytes.Contains(output, []byte(terminal)) {
							t.Fatalf("failure synthesized terminal %q: %s", terminal, output)
						}
					}
					if !streaming && len(output) != 0 {
						t.Fatalf("aggregate failure exposed success body: %s", output)
					}
					if p.Usage == nil || !p.Usage.ProviderReported || p.Usage.TotalTokens != 12 {
						t.Fatalf("failure discarded usage: %+v", p.Usage)
					}
				})
			}
		}
	}
}

func TestProtocolMalformedTrailingUsageInvalidatesPriorBillingEvidence(t *testing.T) {
	for _, dialect := range []wire.Dialect{wire.DialectGemini, wire.DialectClaude, wire.DialectChat, wire.DialectResponses} {
		for _, streaming := range []bool{false, true} {
			if dialect == wire.DialectGemini && !streaming {
				continue
			}
			t.Run(fmt.Sprintf("%s/stream=%t", dialect, streaming), func(t *testing.T) {
				var requests atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					requests.Add(1)
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, `data: {"response":{"candidates":[{"index":0,"content":{"parts":[{"text":"answer"}]},"finishReason":"STOP"}],"usageMetadata":`+protocolUsageJSON+"}}\n\n")
					_, _ = io.WriteString(w, "data: {\"response\":{\"usageMetadata\":{\"promptTokenCount\":\"future-value\",\"candidatesTokenCount\":2,\"totalTokenCount\":12}}}\n\n")
				}))
				defer server.Close()
				input := protocolInput(dialect)
				p := protocolProvider(t, server.URL, input)
				output, apiErr, streamErr := callProtocol(t, p, dialect, streaming, input)
				if apiErr != nil || streamErr != nil || !bytes.Contains(output, []byte("answer")) || requests.Load() != 1 {
					t.Fatalf("usage projection changed delivery: %v %v requests=%d output=%s", apiErr, streamErr, requests.Load(), output)
				}
				if p.Usage == nil || p.Usage.HasProviderUsage() || !p.Usage.ProviderTokenConflict || !p.Usage.BillingDiagnostics["invalid_provider_token_usage"] {
					t.Fatalf("bad trailing usage retained prior exact evidence: %+v", p.Usage)
				}
			})
		}
	}
}

func TestProtocolNativeJSONErrorRetainsReportedUsage(t *testing.T) {
	for _, wrapped := range []bool{false, true} {
		t.Run(fmt.Sprintf("wrapped=%t", wrapped), func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", "application/json")
				body := `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"executed with usage"},"usageMetadata":` + protocolUsageJSON + `}`
				if wrapped {
					body = `{"response":` + body + `}`
				}
				_, _ = io.WriteString(w, body)
			}))
			defer server.Close()
			input := protocolInput(wire.DialectGemini)
			p := protocolProvider(t, server.URL, input)
			output, apiErr, streamErr := callProtocol(t, p, wire.DialectGemini, false, input)
			if apiErr == nil || apiErr.StatusCode != 429 || !apiErr.UpstreamAccepted || !apiErr.ReplayRawResponse || streamErr != nil || len(output) != 0 || requests.Load() != 1 {
				t.Fatalf("HTTP 200 business error changed outcome: apiErr=%v streamErr=%v requests=%d output=%s", apiErr, streamErr, requests.Load(), output)
			}
			if p.Usage == nil || !p.Usage.HasProviderUsage() || p.Usage.PromptTokens != 7 || p.Usage.CompletionTokens != 5 || p.Usage.TotalTokens != 12 {
				t.Fatalf("business error discarded legal usage: %+v", p.Usage)
			}
		})
	}
}

func TestProtocolPostParametersFinalizeExecutionContract(t *testing.T) {
	for _, dialect := range []wire.Dialect{wire.DialectGemini, wire.DialectChat} {
		t.Run(string(dialect)+"/candidate count", func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				var body struct {
					Request struct {
						GenerationConfig struct {
							CandidateCount int `json:"candidateCount"`
						} `json:"generationConfig"`
					} `json:"request"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Request.GenerationConfig.CandidateCount != 2 {
					t.Errorf("post parameter not sent: %+v %v", body, err)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"response\":{\"candidates\":[{\"index\":0,\"content\":{\"parts\":[{\"text\":\"only first candidate\"}]},\"finishReason\":\"STOP\"}]}}\n\n")
			}))
			defer server.Close()
			input := protocolInput(dialect)
			p := protocolProvider(t, server.URL, input)
			post := `{"overwrite":true,"generationConfig":{"candidateCount":2}}`
			p.Channel.CustomParameter = &post
			output, apiErr, streamErr := callProtocol(t, p, dialect, true, input)
			if apiErr != nil || streamErr == nil || requests.Load() != 1 {
				t.Fatalf("missing second candidate accepted: apiErr=%v streamErr=%v requests=%d", apiErr, streamErr, requests.Load())
			}
			if bytes.Contains(output, []byte("data: [DONE]")) || bytes.Contains(output, []byte(`"finish_reason":"stop"`)) {
				t.Fatalf("post candidate count ignored by terminal state: %s", output)
			}
		})
	}
	for _, dialect := range []wire.Dialect{wire.DialectClaude, wire.DialectResponses} {
		for _, tc := range []struct{ name, post, param string }{
			{"image", `{"overwrite":true,"generationConfig":{"responseModalities":["IMAGE"]}}`, "generationConfig.responseModalities"},
			{"server tool", `{"overwrite":true,"tools":[{"codeExecution":{}}]}`, "tools[0].codeExecution"},
		} {
			for _, streaming := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", dialect, tc.name, streaming), func(t *testing.T) {
					var requests atomic.Int32
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { requests.Add(1); w.WriteHeader(500) }))
					defer server.Close()
					input := protocolInput(dialect)
					p := protocolProvider(t, server.URL, input)
					post := tc.post
					p.Channel.CustomParameter = &post
					_, apiErr, streamErr := callProtocol(t, p, dialect, streaming, input)
					if apiErr == nil || apiErr.StatusCode != 400 || apiErr.Param != tc.param || !apiErr.UpstreamNotAttempted || streamErr != nil || requests.Load() != 0 {
						t.Fatalf("unrepresentable post parameter dispatched: apiErr=%v streamErr=%v requests=%d", apiErr, streamErr, requests.Load())
					}
				})
			}
		}
	}
}
