package relay

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"one-api/common"
	"one-api/common/config"
	"one-api/common/requester"
	commonresponses "one-api/common/responses"
	"one-api/model"
	providersBase "one-api/providers/base"
	"one-api/types"
)

func backgroundFixture(t *testing.T, handler http.HandlerFunc, body string) (*relayResponses, *httptest.ResponseRecorder) {
	t.Helper()
	c := setupResponsesWSQuotaFixture(t, 100000)
	if err := model.DB.AutoMigrate(&model.Task{}); err != nil {
		t.Fatal(err)
	}
	if err := model.DB.Model(&model.Token{}).Where("id = 1").Updates(map[string]any{"status": config.TokenStatusEnabled, "expired_time": -1}).Error; err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = client })
	proxy := ""
	if err := model.DB.Model(&model.Channel{}).Where("id = 17").Updates(map[string]any{"base_url": server.URL, "proxy": proxy, "other": `{"responses_stored_lifecycle":true}`}).Error; err != nil {
		t.Fatal(err)
	}
	channel := &model.Channel{}
	if err := model.DB.First(channel, 17).Error; err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	c.Writer = nil
	fresh, _ := gin.CreateTestContext(recorder)
	fresh.Keys = c.Keys
	fresh.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	enableResponsesTestDeadline(fresh)
	envelope, err := commonresponses.ParseRawEnvelope([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	common.SetReusableRequestBody(fresh, []byte(body))
	provider, mapped, err := prepareProviderForChannel(fresh, "gpt-5", channel)
	if err != nil {
		t.Fatal(err)
	}
	relay := NewRelayResponses(fresh)
	relay.provider = provider
	relay.modelName = mapped
	relay.originalModel = "gpt-5"
	relay.rawEnvelope = envelope
	relay.responsesRequest = envelope.Projection
	relay.selectedDataPath = providersBase.DataPathExactWire
	t.Cleanup(func() { waitBackgroundObserver(t, fresh) })
	return relay, recorder
}

func TestBackgroundResponsesCreatePollUsesOneTaskAndPreservesBody(t *testing.T) {
	body := `{"model":"gpt-5","background":true,"store":false,"future":{"mode":[1e0,"x"]},"input":"hello"}`
	var creates, polls atomic.Int32
	r, rec := backgroundFixture(t, func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if req.Method == http.MethodPost {
			creates.Add(1)
			got, _ := io.ReadAll(req.Body)
			if string(got) != body {
				t.Errorf("request changed: %s", got)
			}
			_, _ = io.WriteString(w, `{"id":"resp_background","status":"queued","future":{"result":true}}`)
			return
		}
		polls.Add(1)
		_, _ = io.WriteString(w, `{"id":"resp_background","status":"completed","model":"gpt-5","usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}`)
	}, body)
	// Use token pricing so a provider token result is priceable rather than an
	// invented successful-operation charge.
	model.PricingInstance.Prices["gpt-5"] = &model.Price{Model: "gpt-5", Type: model.TokensPriceType, Input: 1, Output: 1}
	apiErr, done := RelayHandler(r)
	if apiErr != nil || !done {
		t.Fatalf("create err=%+v done=%v body=%s", apiErr, done, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"future":{"result":true}`) {
		t.Fatalf("raw response changed: %s", rec.Body.String())
	}
	waitBackgroundObserver(t, r.c)
	owner, err := model.GetResponseOwner(context.Background(), "resp_background", 1)
	if err != nil || owner.TaskOwnerID == nil {
		t.Fatalf("owner missing: %+v %v", owner, err)
	}
	task, err := model.GetBackgroundResponseTask(context.Background(), *owner.TaskOwnerID)
	if err != nil {
		t.Fatal(err)
	}
	if task.ProviderState != model.TaskProviderStateAccepted {
		t.Fatalf("not accepted: %+v", task)
	}
	var count int64
	model.DB.Model(&model.Task{}).Count(&count)
	if count != 1 {
		t.Fatalf("owners=%d", count)
	}
	// The initiating HTTP request no longer exists. Polling only needs the Task.
	if err = pollBackgroundResponse(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if task.ProviderState != model.TaskProviderStateClosed || task.ChargedQuota == nil || *task.ChargedQuota <= 0 {
		t.Fatalf("usage not finalized: %+v data=%s", task, task.Data)
	}
	charged := *task.ChargedQuota
	if err = pollBackgroundResponse(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if creates.Load() != 1 || polls.Load() != 1 || *task.ChargedQuota != charged {
		t.Fatalf("duplicate work: post=%d get=%d task=%+v", creates.Load(), polls.Load(), task)
	}
}

func TestBackgroundResponsesSSEAcceptsBeforeIDDeliveryAndKeepsExtensions(t *testing.T) {
	wire := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_stream_bg\",\"status\":\"queued\"}}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_stream_bg\",\"status\":\"completed\"}}\n\nevent: future\ndata: {\"value\":1}\n\n"
	r, rec := backgroundFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, wire)
	}, `{"model":"gpt-5","background":true,"stream":true,"store":false,"input":"hello"}`)
	apiErr, done := RelayHandler(r)
	if apiErr != nil || !done {
		t.Fatalf("stream error=%+v done=%v wire=%s", apiErr, done, rec.Body.String())
	}
	if rec.Body.String() != wire {
		t.Fatalf("wire mismatch: %q", rec.Body.String())
	}
	waitBackgroundObserver(t, r.c)
	owner, err := model.GetResponseOwner(context.Background(), "resp_stream_bg", 1)
	if err != nil || owner.TaskOwnerID == nil {
		t.Fatalf("owner=%+v err=%v", owner, err)
	}
	task, err := model.GetBackgroundResponseTask(context.Background(), *owner.TaskOwnerID)
	if err != nil || task.ProviderState != model.TaskProviderStateClosed {
		t.Fatalf("task=%+v err=%v", task, err)
	}
}

func TestStoredResponsesCancelAndDeletedOwnerRemainTransparent(t *testing.T) {
	payload := `{"future":{"reason":"client"}}`
	owner, _, calls := setupStoredResponsesHandlerTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			got, _ := io.ReadAll(r.Body)
			if r.URL.Path != "/v1/responses/resp_lifecycle/cancel" || string(got) != payload {
				t.Errorf("cancel changed: %s %s", r.URL.Path, got)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_lifecycle","status":"cancelled"}`)
	})
	if err := model.TombstoneResponseOwner(context.Background(), owner.ResponseID, owner.UserID); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{http.MethodGet, http.MethodDelete, http.MethodPost} {
		path := "/v1/responses/resp_lifecycle"
		if method == http.MethodPost {
			path += "/cancel"
		}
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(method, path, strings.NewReader(payload))
		c.Params = gin.Params{{Key: "response_id", Value: owner.ResponseID}}
		c.Set("id", owner.UserID)
		c.Set("token_id", owner.TokenID)
		StoredResponses(c)
		if rec.Code != http.StatusOK {
			t.Fatalf("method=%s status=%d body=%s", method, rec.Code, rec.Body.String())
		}
	}
	if atomic.LoadInt32(calls) != 3 {
		t.Fatalf("calls=%d", atomic.LoadInt32(calls))
	}
}

func TestBackgroundEvidenceRoundTripRetainsTrustAndNoOutput(t *testing.T) {
	response := &types.OpenAIResponsesResponses{}
	if err := response.DecodeCapturedProviderJSON([]byte(`{"id":"resp_e","usage":{"input_tokens":0,"output_tokens":2,"total_tokens":2},"output":[{"type":"message","content":[{"type":"output_text","text":"private output"}]}]}`)); err != nil {
		t.Fatal(err)
	}
	response.Usage.MarkProviderReported()
	usage := response.Usage.ToOpenAIUsage()
	data := backgroundResponseData{Model: "gpt-5", Group: "default", Evidence: captureBackgroundUsage(response.ID, usage)}
	raw, err := encodeBackgroundTaskData(data)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "private output") {
		t.Fatal("stored output")
	}
	var decoded backgroundResponseData
	if err = json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	restored := decoded.Evidence.restore()
	if !restored.HasProviderBaseUsage() || restored.PromptTokens != 0 || restored.CompletionTokens != 2 {
		t.Fatalf("lost provider evidence: %+v", restored)
	}
}

func TestBackgroundPollTrackingTimeoutDoesNotGenerateOrInventProviderFailure(t *testing.T) {
	var calls atomic.Int32
	r, _ := backgroundFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, `{"id":"resp_timeout","status":"queued"}`)
	}, `{"model":"gpt-5","background":true,"store":false,"input":"hello"}`)
	if apiErr, _ := RelayHandler(r); apiErr != nil {
		t.Fatal(apiErr)
	}
	waitBackgroundObserver(t, r.c)
	owner, _ := model.GetResponseOwner(context.Background(), "resp_timeout", 1)
	task, _ := model.GetBackgroundResponseTask(context.Background(), *owner.TaskOwnerID)
	old := time.Now().Add(-25 * time.Hour).Unix()
	task.AcceptanceRecordedAt = &old
	if err := pollBackgroundResponse(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || task.Status != model.TaskStatusUnknown || task.SettlementDecision != "cancel" {
		t.Fatalf("timeout changed upstream semantics: calls=%d task=%+v", calls.Load(), task)
	}
}

func TestBackgroundEmptyLateObservationPreservesDurableEvidence(t *testing.T) {
	r, _ := backgroundFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"resp_evidence","status":"queued"}`)
	}, `{"model":"gpt-5","background":true,"store":false,"input":"hello"}`)
	if apiErr, _ := RelayHandler(r); apiErr != nil {
		t.Fatal(apiErr)
	}
	waitBackgroundObserver(t, r.c)
	owner, _ := model.GetResponseOwner(context.Background(), "resp_evidence", 1)
	task, _ := model.GetBackgroundResponseTask(context.Background(), *owner.TaskOwnerID)
	response := &types.OpenAIResponsesResponses{}
	_ = response.DecodeCapturedProviderJSON([]byte(`{"id":"resp_evidence","status":"in_progress","usage":{"input_tokens":3,"output_tokens":4,"total_tokens":7}}`))
	observeBackgroundTask(context.Background(), task, response, nil)
	// Same stream holds an older in-memory snapshot; a current-version refresh
	// must not authorize erasing provider counters already saved by the poller.
	observeBackgroundTask(context.Background(), task, &types.OpenAIResponsesResponses{ID: "resp_evidence", Status: "queued"}, &types.Usage{})
	stored, err := model.GetBackgroundResponseTask(context.Background(), task.OwnerID)
	if err != nil {
		t.Fatal(err)
	}
	usage := backgroundTaskData(stored).Evidence.restore()
	if !usage.HasProviderBaseUsage() || usage.TotalTokens != 7 {
		t.Fatalf("lost evidence: %+v", usage)
	}
}

func waitBackgroundObserver(t *testing.T, c *gin.Context) {
	t.Helper()
	if value, ok := c.Get(responsesBackgroundObserverDoneContextKey); ok {
		select {
		case <-value.(chan struct{}):
		case <-time.After(10 * time.Second):
			t.Fatal("background accounting observer did not finish")
		}
	}
}

func TestBackgroundOriginalBillingModelPolicySurvivesPolling(t *testing.T) {
	r, _ := backgroundFixture(t, func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if req.Method == http.MethodPost {
			_, _ = io.WriteString(w, `{"id":"resp_price","status":"queued"}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"resp_price","status":"completed","model":"upstream-expensive","usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}`)
	}, `{"model":"gpt-5","background":true,"store":false,"input":"hello"}`)
	model.PricingInstance.Prices["gpt-5"] = &model.Price{Model: "gpt-5", Type: model.TokensPriceType, Input: 1, Output: 1}
	model.PricingInstance.Prices["upstream-expensive"] = &model.Price{Model: "upstream-expensive", Type: model.TokensPriceType, Input: 100, Output: 100}
	r.c.Set("billing_original_model", true)
	if apiErr, _ := RelayHandler(r); apiErr != nil {
		t.Fatal(apiErr)
	}
	waitBackgroundObserver(t, r.c)
	owner, _ := model.GetResponseOwner(context.Background(), "resp_price", 1)
	task, _ := model.GetBackgroundResponseTask(context.Background(), *owner.TaskOwnerID)
	if !backgroundTaskData(task).BillingOriginalModel {
		t.Fatal("lost model policy")
	}
	if err := pollBackgroundResponse(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if task.ChargedQuota == nil || *task.ChargedQuota <= 0 || *task.ChargedQuota > 10 {
		t.Fatalf("used reported expensive model price: %+v", task)
	}
}

func TestBackgroundCancelAndRecoveryObserveSameSettlement(t *testing.T) {
	usage := `{"id":"resp_cancel","status":"cancelled","model":"gpt-5","usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}`
	wire := "data: {\"type\":\"response.completed\",\"response\":" + usage + "}\n\nevent: future\ndata: {}\n\n"
	var creates atomic.Int32
	r, _ := backgroundFixture(t, func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost && req.URL.Path == "/v1/responses" {
			creates.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"resp_cancel","status":"queued"}`)
			return
		}
		if strings.HasSuffix(req.URL.Path, "/cancel") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, usage)
			return
		}
		if req.URL.RawQuery != "stream=true&starting_after=opaque%2Bcursor" {
			t.Errorf("cursor changed: %s", req.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, wire)
	}, `{"model":"gpt-5","background":true,"store":false,"input":"hello"}`)
	model.PricingInstance.Prices["gpt-5"] = &model.Price{Model: "gpt-5", Type: model.TokensPriceType, Input: 1, Output: 1}
	if apiErr, _ := RelayHandler(r); apiErr != nil {
		t.Fatal(apiErr)
	}
	waitBackgroundObserver(t, r.c)
	owner, _ := model.GetResponseOwner(context.Background(), "resp_cancel", 1)
	var charged int64
	for _, method := range []string{http.MethodPost, http.MethodGet} {
		path := "/v1/responses/resp_cancel/cancel"
		if method == http.MethodGet {
			path = "/v1/responses/resp_cancel?stream=true&starting_after=opaque%2Bcursor"
		}
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		enableResponsesTestDeadline(c)
		c.Request = httptest.NewRequest(method, path, nil)
		c.Params = gin.Params{{Key: "response_id", Value: owner.ResponseID}}
		c.Set("id", 1)
		c.Set("token_id", 1)
		StoredResponses(c)
		waitBackgroundObserver(t, c)
		if rec.Code != 200 {
			t.Fatalf("lifecycle status=%d body=%s", rec.Code, rec.Body.String())
		}
		task, _ := model.GetBackgroundResponseTask(context.Background(), *owner.TaskOwnerID)
		if task.ChargedQuota == nil || *task.ChargedQuota <= 0 {
			t.Fatalf("cancel lost usage: %+v", task)
		}
		if method == http.MethodPost {
			charged = *task.ChargedQuota
		} else if *task.ChargedQuota != charged || rec.Body.String() != wire {
			t.Fatalf("recovery changed charge/wire: task=%+v wire=%q", task, rec.Body.String())
		}
	}
	if creates.Load() != 1 {
		t.Fatalf("replayed create=%d", creates.Load())
	}
}

func TestBackgroundSlowAccountingDoesNotDelayStreamDelivery(t *testing.T) {
	wire := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_slow_observer\",\"status\":\"queued\"}}\n\nevent: future\ndata: {}\n\n"
	r, rec := backgroundFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, wire)
	}, `{"model":"gpt-5","background":true,"stream":true,"store":false,"input":"hello"}`)
	release := make(chan struct{})
	if err := model.DB.Callback().Query().Before("gorm:query").Register("background:slow_observer", func(tx *gorm.DB) {
		if where, ok := tx.Statement.Clauses["WHERE"].Expression.(clause.Where); ok && tx.Statement.Table == "tasks" {
			for _, expression := range where.Exprs {
				if predicate, ok := expression.(clause.Expr); ok && predicate.SQL == "owner_id = ? AND platform = ?" {
					<-release
				}
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	defer model.DB.Callback().Query().Remove("background:slow_observer")
	completed := make(chan *types.OpenAIErrorWithStatusCode, 1)
	go func() { apiErr, _ := RelayHandler(r); completed <- apiErr }()
	select {
	case apiErr := <-completed:
		close(release)
		if apiErr != nil || rec.Body.String() != wire {
			t.Fatalf("delivery=%q error=%+v", rec.Body.String(), apiErr)
		}
	case <-time.After(2 * time.Second):
		close(release)
		<-completed
		t.Fatal("accounting query blocked raw delivery")
	}
	waitBackgroundObserver(t, r.c)
}

type backgroundDisconnectWriter struct {
	gin.ResponseWriter
	cancel context.CancelFunc
}

func (w *backgroundDisconnectWriter) Write(raw []byte) (int, error) {
	n, err := w.ResponseWriter.Write(raw)
	w.cancel()
	return n, err
}
func (w *backgroundDisconnectWriter) WriteString(raw string) (int, error) {
	return w.Write([]byte(raw))
}
func (*backgroundDisconnectWriter) SetWriteDeadline(time.Time) error { return nil }
func (w *backgroundDisconnectWriter) Unwrap() http.ResponseWriter    { return w.ResponseWriter }

func TestBackgroundSSEDisconnectKeepsAcceptedPollOwner(t *testing.T) {
	var creates, polls atomic.Int32
	r, _ := backgroundFixture(t, func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost {
			creates.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_disconnect\",\"status\":\"queued\"}}\n\n")
			w.(http.Flusher).Flush()
			<-req.Context().Done()
			return
		}
		polls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_disconnect","status":"completed"}`)
	}, `{"model":"gpt-5","background":true,"stream":true,"store":false,"input":"hello"}`)
	ctx, cancel := context.WithCancel(r.c.Request.Context())
	defer cancel()
	r.c.Request = r.c.Request.WithContext(ctx)
	r.c.Writer = &backgroundDisconnectWriter{ResponseWriter: r.c.Writer, cancel: cancel}
	_, done := RelayHandler(r)
	if !done {
		t.Fatal("submitted background may not retry")
	}
	waitBackgroundObserver(t, r.c)
	owner, err := model.GetResponseOwner(context.Background(), "resp_disconnect", 1)
	if err != nil {
		t.Fatal(err)
	}
	task, err := model.GetBackgroundResponseTask(context.Background(), *owner.TaskOwnerID)
	if err != nil {
		t.Fatal(err)
	}
	if task.ProviderState != model.TaskProviderStateAccepted {
		t.Fatalf("disconnect closed background work: %+v", task)
	}
	if err := pollBackgroundResponse(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if task.ProviderState != model.TaskProviderStateClosed || creates.Load() != 1 || polls.Load() != 1 {
		t.Fatalf("background did not resume tracking: %+v posts=%d polls=%d", task, creates.Load(), polls.Load())
	}
}
