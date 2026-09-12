package relay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"one-api/common/config"
	"one-api/common/groupctx"
	"one-api/common/requester"
	commonresponses "one-api/common/responses"
	"one-api/model"
	providersBase "one-api/providers/base"
	"one-api/providers/openai"
	"one-api/types"
)

type responsesGatedReadBody struct {
	*strings.Reader
	gate   <-chan struct{}
	closed chan struct{}
	once   sync.Once
	ending error
}

func (b *responsesGatedReadBody) Read(p []byte) (int, error) {
	if b.Len() > 0 {
		return b.Reader.Read(p)
	}
	select {
	case <-b.gate:
		return 0, b.ending
	case <-b.closed:
		return 0, io.ErrClosedPipe
	}
}
func (b *responsesGatedReadBody) Close() error { b.once.Do(func() { close(b.closed) }); return nil }

type responsesHTTPReaderProvider struct {
	streamAffinityResponsesProvider
	body io.ReadCloser
}

func (p *responsesHTTPReaderProvider) CreateResponsesStream(context.Context, *commonresponses.Request) (commonresponses.EventStream, *types.OpenAIErrorWithStatusCode) {
	handler := &openai.OpenAIResponsesStreamHandler{Usage: p.Usage}
	stream, err := requester.RequestNoTrimStreamWithEmitterOptions[string](nil, &http.Response{Body: p.body}, handler.HandlerResponsesStreamWithEmitter, requester.StreamReadOptions{SSELines: true})
	return commonresponses.NewEventStream(stream, handler.ObserveResponsesEvent), err
}

type responsesWriteSignal struct {
	gin.ResponseWriter
	entered chan<- struct{}
	once    sync.Once
}

func (w *responsesWriteSignal) Unwrap() http.ResponseWriter       { return w.ResponseWriter }
func (w *responsesWriteSignal) WriteString(s string) (int, error) { return w.Write([]byte(s)) }
func (w *responsesWriteSignal) Write(p []byte) (int, error) {
	if len(p) > 1<<20 {
		w.once.Do(func() { w.entered <- struct{}{} })
	}
	return w.ResponseWriter.Write(p)
}

func TestResponsesHTTPTransportEndsAndSettlesOnce(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		for _, mode := range []string{"eof", "reset", "unsafe_tail", "blocked_source_error", "blocked_cancel", "blocked_deadline"} {
			t.Run(fmt.Sprintf("h2=%t/%s", h2, mode), func(t *testing.T) {
				setupResponsesHTTPBillingFixture(t)
				prefix := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_transport\"}}\r\r" +
					"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"search_transport\",\"type\":\"web_search_call\",\"status\":\"completed\",\"action\":{\"type\":\"search\"}}}\r\r"
				wire := prefix
				blocked := strings.HasPrefix(mode, "blocked_")
				if blocked {
					// 大注释只制造真实 socket 背压；JSON 投影与脱敏由独立用例验证，
					// 避免 race 插桩的解析开销消耗本测试的 I/O deadline。
					wire += ": " + strings.Repeat("x", 8<<20) + "\r\r"
				}
				if mode == "eof" {
					wire += "data: [DONE]\r\rdata: {\"type\":\"response.future\",\"n\":9007199254740993}\r\r"
				} else if mode == "unsafe_tail" {
					wire += `data: {"type":"response.completed","response":{"id":"resp_transport","usage":{"input_tokens":999,"output_tokens":999,"total_tokens":1998},"account_id":"acct-secret-transport"`
				}
				gate := make(chan struct{})
				ending := error(io.ErrUnexpectedEOF)
				if mode == "eof" {
					close(gate)
				}
				if mode == "eof" || mode == "unsafe_tail" {
					ending = io.EOF
				}
				body := &responsesGatedReadBody{Reader: strings.NewReader(wire), gate: gate, closed: make(chan struct{}), ending: ending}
				entered := make(chan struct{}, 1)
				cancelRequest := make(chan context.CancelFunc, 1)
				type completion struct {
					aborted bool
					apiErr  *types.OpenAIErrorWithStatusCode
				}
				finished := make(chan completion, 1)
				engine := gin.New()
				engine.POST("/v1/responses", func(c *gin.Context) {
					ctx, cancel := context.WithCancel(c.Request.Context())
					if mode == "blocked_deadline" {
						cancel()
						ctx, cancel = context.WithTimeout(c.Request.Context(), 2*time.Second)
					}
					defer cancel()
					cancelRequest <- cancel
					c.Request = c.Request.WithContext(ctx)
					c.Writer = &responsesWriteSignal{ResponseWriter: c.Writer, entered: entered}
					c.Set("id", 1)
					c.Set("token_id", 1)
					c.Set("group_ratio", 1.0)
					groupctx.SetRoutingGroup(c, "default", groupctx.RoutingGroupSourceUserGroup)
					provider := &responsesHTTPReaderProvider{streamAffinityResponsesProvider: streamAffinityResponsesProvider{BaseProvider: providersBase.BaseProvider{Channel: &model.Channel{Id: 17, Type: config.ChannelTypeOpenAI}, Context: c}}, body: body}
					envelope, parseErr := commonresponses.ParseRawEnvelope([]byte(`{"model":"` + issue048HTTPModel + `","input":"hello","stream":true,"store":false}`))
					if parseErr != nil {
						panic(parseErr)
					}
					relay := &relayResponses{relayBase: relayBase{c: c, provider: provider, originalModel: issue048HTTPModel, modelName: issue048HTTPModel}, responsesRequest: envelope.Projection, rawEnvelope: envelope, operation: responsesOperationCreate}
					var apiErr *types.OpenAIErrorWithStatusCode
					defer func() {
						recovered := recover()
						finished <- completion{aborted: recovered == http.ErrAbortHandler, apiErr: apiErr}
						if recovered != nil {
							panic(recovered)
						}
					}()
					apiErr, _ = RelayHandler(relay)
					if apiErr != nil {
						relay.HandleJsonError(apiErr)
					}
				})
				server := httptest.NewUnstartedServer(engine)
				server.EnableHTTP2 = h2
				if h2 {
					server.StartTLS()
				} else {
					server.Start()
				}
				defer server.Close()
				client := server.Client()
				client.Timeout = 6 * time.Second
				if transport, ok := client.Transport.(*http.Transport); ok {
					transport.ForceAttemptHTTP2 = h2
				}
				resp, err := client.Post(server.URL+"/v1/responses", "application/json", strings.NewReader(`{}`))
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				if (resp.ProtoMajor == 2) != h2 {
					t.Fatalf("wrong HTTP protocol: %s", resp.Proto)
				}
				cancel := <-cancelRequest
				if blocked {
					select {
					case <-entered:
					case <-time.After(3 * time.Second):
						t.Fatal("large downstream write never started")
					}
					if mode == "blocked_cancel" {
						cancel()
					}
					if mode == "blocked_source_error" {
						close(gate)
					}
				} else if mode == "reset" || mode == "unsafe_tail" {
					first := make([]byte, len(prefix))
					if _, err := io.ReadFull(resp.Body, first); err != nil || string(first) != prefix {
						t.Fatalf("prefix not delivered: %q %v", first, err)
					}
					close(gate)
				}
				var result completion
				if blocked {
					select {
					case result = <-finished:
					case <-time.After(4 * time.Second):
						t.Fatal("downstream write blocked handler cleanup")
					}
				}
				got, readErr := io.ReadAll(resp.Body)
				if !blocked {
					select {
					case result = <-finished:
					case <-time.After(time.Second):
						t.Fatal("handler did not finish")
					}
				}
				if mode == "eof" || mode == "unsafe_tail" {
					wantBody := wire
					if mode == "unsafe_tail" {
						wantBody = strings.TrimPrefix(wire, prefix)
					}
					if readErr != nil || result.aborted || result.apiErr != nil || string(got) != wantBody {
						t.Fatalf("normal EOF changed: err=%v result=%+v bytes=%q", readErr, result, got)
					}
				} else if readErr == nil || !result.aborted {
					t.Fatalf("truncation looked successful: read=%v result=%+v", readErr, result)
				}

				select {
				case <-body.closed:
				default:
					t.Fatal("upstream body not closed")
				}
				var logs []model.Log
				if err := model.DB.Find(&logs).Error; err != nil {
					t.Fatal(err)
				}
				if len(logs) != 1 || logs[0].Quota <= 0 {
					t.Fatalf("expected one provider-usage settlement: %+v", logs)
				}
				if mode == "unsafe_tail" {
					tokenBilling, ok := logs[0].Metadata.Data()["token_billing"].(map[string]any)
					if !ok || tokenBilling["status"] != "missing_evidence" || logs[0].Quota != 5000 {
						t.Fatalf("尾部进入了 token 计费，或独立工具费用丢失：quota=%d token_billing=%+v", logs[0].Quota, tokenBilling)
					}
				}
				var user model.User
				var token model.Token
				if err := model.DB.First(&user, 1).Error; err != nil {
					t.Fatal(err)
				}
				if err := model.DB.First(&token, 1).Error; err != nil {
					t.Fatal(err)
				}
				if user.Quota != 100000-logs[0].Quota || token.RemainQuota != user.Quota {
					t.Fatalf("settlement duplicated or lost: user=%d token=%d charge=%d", user.Quota, token.RemainQuota, logs[0].Quota)
				}
				if mode != "eof" && errors.Is(readErr, io.EOF) {
					t.Fatal("abnormal response returned normal EOF")
				}
			})
		}
	}
}

func TestResponsesSSELineEndingsPreserveSecurityAndIncompleteTail(t *testing.T) {
	for _, separator := range []string{"\r", "\n", "\r\n"} {
		for _, complete := range []bool{false, true} {
			t.Run(fmt.Sprintf("separator=%q/complete=%t", separator, complete), func(t *testing.T) {
				ctx, recorder := responsesOwnerTestContext(1, 1)
				usage := &types.Usage{}
				handler := &openai.OpenAIResponsesStreamHandler{Usage: usage}
				wire := "event: response.completed" + separator + `data: {"type":"response.completed","response":{"id":"resp_safe","account_id":"acct-secret","status":"completed","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}},"n":9007199254740993}`
				if complete {
					wire += separator + separator
				}
				stream, err := requester.RequestNoTrimStreamWithEmitterOptions[string](nil, &http.Response{Body: io.NopCloser(strings.NewReader(wire))}, handler.HandlerResponsesStreamWithEmitter, requester.StreamReadOptions{SSELines: true})
				if err != nil {
					t.Fatal(err)
				}
				_, apiErr := responseNativeResponsesStreamClient(ctx, commonresponses.NewEventStream(stream, handler.ObserveResponsesEvent), commonresponses.NewStreamObserver())
				if apiErr != nil {
					t.Fatal(apiErr)
				}
				body := recorder.Body.String()
				if body != wire {
					t.Fatalf("SSE security or number changed: %q", body)
				}
				if complete {
					if !usage.HasProviderUsage() || usage.TotalTokens != 3 || !recorder.Flushed || !strings.HasSuffix(body, separator+separator) {
						t.Fatalf("complete event not delivered/observed: %q %+v", body, usage)
					}
				} else if usage.ProviderReported || usage.TotalTokens != 0 || strings.HasSuffix(body, "\r") || strings.HasSuffix(body, "\n") {
					t.Fatalf("EOF tail was promoted to an event: %q %+v", body, usage)
				}
			})
		}
	}
}

func TestResponsesSSEBestEffortFailurePreservesDelivery(t *testing.T) {
	for _, separator := range []string{"\r", "\n", "\r\n"} {
		for _, completeEvent := range []bool{false, true} {
			for _, committed := range []bool{false, true} {
				t.Run(fmt.Sprintf("separator=%q/complete=%t/committed=%t", separator, completeEvent, committed), func(t *testing.T) {
					ctx, recorder := responsesOwnerTestContext(1, 1)
					usage := &types.Usage{}
					handler := &openai.OpenAIResponsesStreamHandler{Usage: usage}
					prefix := ""
					if committed {
						prefix = `data: {"type":"response.created","response":{"id":"resp_safe"}}` + separator + separator
					}
					wire := prefix + `data: {"type":"response.in_progress","response":{"id":"resp_safe","account_id":"acct-secret-tail"`
					if completeEvent {
						wire += separator + separator
					}
					stream, err := requester.RequestNoTrimStreamWithEmitterOptions[string](nil, &http.Response{Body: io.NopCloser(strings.NewReader(wire))}, handler.HandlerResponsesStreamWithEmitter, requester.StreamReadOptions{SSELines: true})
					if err != nil {
						t.Fatal(err)
					}
					_, apiErr := responseNativeResponsesStreamClient(ctx, commonresponses.NewEventStream(stream, handler.ObserveResponsesEvent), commonresponses.NewStreamObserver())
					if apiErr != nil || recorder.Body.String() != wire {
						t.Fatalf("脱敏干扰原文交付: %q err=%v", recorder.Body.String(), apiErr)
					}
					if usage.HasProviderUsage() {
						t.Fatal("无效 JSON 成为了计费证据")
					}
				})
			}
		}
	}
}
