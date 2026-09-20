package relay

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"one-api/common/config"
	"one-api/common/requester"
	"one-api/model"
	"one-api/relay/relay_util"
)

func batchFixture(t *testing.T, handler http.HandlerFunc) (*model.Channel, func(string, string, io.Reader) (*gin.Context, *httptest.ResponseRecorder)) {
	t.Helper()
	base := setupResponsesWSQuotaFixture(t, 1000000)
	if err := model.DB.AutoMigrate(&model.ResourceOwner{}, &model.Task{}); err != nil {
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
	if err := model.DB.Model(&model.Channel{}).Where("id = 17").Updates(map[string]any{"base_url": server.URL, "proxy": ""}).Error; err != nil {
		t.Fatal(err)
	}
	channel := &model.Channel{}
	if err := model.DB.First(channel, 17).Error; err != nil {
		t.Fatal(err)
	}
	snapshot := snapshotChannelGroup()
	model.ChannelGroup = buildRealtimeTestChannelGroupForChannels(channel)
	t.Cleanup(func() { restoreChannelGroup(snapshot) })
	oldLog := config.LogConsumeEnabled
	config.LogConsumeEnabled = false
	t.Cleanup(func() { config.LogConsumeEnabled = oldLog })
	setBatchTestPrice(t, 1, 1)
	seedResourceHTTPOwner(t, channel, "file", "file-input", 1)
	return channel, func(method, path string, body io.Reader) (*gin.Context, *httptest.ResponseRecorder) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		for k, v := range base.Keys {
			c.Set(k, v)
		}
		c.Request = httptest.NewRequest(method, path, body)
		enableResponsesTestDeadline(c)
		c.Request.Header.Set("Content-Type", "application/json")
		return c, rec
	}
}
func setBatchTestPrice(t *testing.T, input, output float64) {
	t.Helper()
	head, err := model.ReadPublicationVersion(context.Background(), model.DB, model.PublicationOwnerPrice)
	if err != nil {
		t.Fatal(err)
	}
	if err = model.PricingInstance.UpdatePriceAtVersion("gpt-5", &model.Price{Model: "gpt-5", Type: model.TokensPriceType, Input: input, Output: output}, true, head); err != nil {
		t.Fatal(err)
	}
}
func batchTestTask(t *testing.T) *model.Task {
	t.Helper()
	var task model.Task
	if err := model.DB.Where("platform = ?", model.TaskPlatformOpenAIBatch).First(&task).Error; err != nil {
		t.Fatal(err)
	}
	return &task
}

func TestBatchCreatePollPartialEvidenceUsesCurrentPriceOnce(t *testing.T) {
	post := `{ "input_file_id":"file-input", "endpoint":"/v1/chat/completions", "completion_window":"future-window", "unknown":{"v":1e0} }`
	input := "{\"custom_id\":\"a\",\"method\":\"POST\",\"url\":\"/v1/chat/completions\",\"body\":{\"model\":\"gpt-5\",\"messages\":[],\"future\":{\"parameters\":{\"file_id\":\"business\"}}}}\n" +
		"{\"custom_id\":\"b\",\"method\":\"POST\",\"url\":\"/v1/chat/completions\",\"body\":{\"model\":\"gpt-5\",\"messages\":[]}}\n"
	resultBody := `{"id":"chat-a","model":"gpt-5","usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`
	output := `{"custom_id":"a","response":{"status_code":200,"body":` + resultBody + "}}\n" + `{"custom_id":"b","response":{"status_code":200,"body":{"id":"chat-b","future":true}}}` + "\n"
	var creates, polls atomic.Int32
	_, makeContext := batchFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/files/file-input/content":
			io.WriteString(w, input)
		case "/v1/batches":
			creates.Add(1)
			raw, _ := io.ReadAll(r.Body)
			if string(raw) != post {
				t.Errorf("POST rewritten: %s", raw)
			}
			io.WriteString(w, `{"id":"batch-1","status":"validating","future":true}`)
		case "/v1/batches/batch-1":
			polls.Add(1)
			io.WriteString(w, `{"id":"batch-1","status":"completed","output_file_id":"file-output","error_file_id":"file-errors"}`)
		case "/v1/files/file-output/content":
			io.WriteString(w, output)
		case "/v1/files/file-errors/content":
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"error":{"message":"client deleted file"}}`)
		default:
			t.Errorf("unexpected upstream request: %s %s", r.Method, r.URL)
			w.WriteHeader(500)
		}
	})
	c, rec := makeContext("POST", "/v1/batches", strings.NewReader(post))
	BatchRelay(c)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"future":true`) {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	task := batchTestTask(t)
	if task.ProviderState != model.TaskProviderStateAccepted {
		t.Fatalf("not accepted: %+v", task)
	}
	setBatchTestPrice(t, 3, 5)
	expectedContext := backgroundTaskContext(context.Background(), task, backgroundResponseData{Group: "default"})
	p, a, err := batchProvider(expectedContext, 17)
	_ = p
	if err != nil {
		t.Fatal(err)
	}
	quota, err := relay_util.NewPricedQuota(expectedContext, "gpt-5", 0)
	if err != nil {
		t.Fatal(err)
	}
	expected := quota.EvaluateProviderUsage(a.ExtractBatchUsage("/v1/chat/completions", []byte(resultBody)))
	if err := pollOpenAIBatch(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	task = batchTestTask(t)
	if task.ProviderState != model.TaskProviderStateAccepted || task.ChargedQuota != nil {
		t.Fatalf("unobserved error File closed owner: %+v", task)
	}
	saved, err := batchData(task)
	if err != nil || saved.PendingEvidence["a"] == nil || saved.TerminalStatus != "completed" {
		t.Fatalf("partial evidence was not retained: %+v %v", saved, err)
	}
	if err := model.DB.Model(task).Update("acceptance_recorded_at", time.Now().Add(-model.BatchTrackingWindow-time.Second).Unix()).Error; err != nil {
		t.Fatal(err)
	}
	task = batchTestTask(t)
	if err := pollOpenAIBatch(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	task = batchTestTask(t)
	if task.ChargedQuota == nil || *task.ChargedQuota != expected.FinalQuota || task.SettlementDecision != "confirm" {
		t.Fatalf("partial/current evidence settlement: task=%+v expected=%+v", task, expected)
	}
	if err := pollOpenAIBatch(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if creates.Load() != 1 || polls.Load() != 1 {
		t.Fatalf("replayed work post=%d poll=%d", creates.Load(), polls.Load())
	}
	var count int64
	model.DB.Model(&model.Task{}).Count(&count)
	if count != 1 {
		t.Fatalf("per-item task created: %d", count)
	}
	if _, err := model.GetResourceOwner(context.Background(), "file", "file-output", 1); err != nil {
		t.Fatal("output owner missing", err)
	}
}

func TestBatchMappingRejectsBeforeTaskAndCreate(t *testing.T) {
	var creates atomic.Int32
	channel, makeContext := batchFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/files/file-input/content" {
			io.WriteString(w, `{"custom_id":"a","method":"POST","url":"/v1/chat/completions","body":{"model":"gpt-5"}}`)
			return
		}
		creates.Add(1)
	})
	mapping := `{"gpt-5":"mapped-model"}`
	if err := model.DB.Model(channel).Update("model_mapping", mapping).Error; err != nil {
		t.Fatal(err)
	}
	c, rec := makeContext("POST", "/v1/batches", strings.NewReader(`{"input_file_id":"file-input","endpoint":"/v1/chat/completions"}`))
	BatchRelay(c)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "rewriting") {
		t.Fatalf("mapping gate: %d %s", rec.Code, rec.Body)
	}
	var count int64
	model.DB.Model(&model.Task{}).Count(&count)
	if creates.Load() != 0 || count != 0 {
		t.Fatalf("provider work or Task before admission: %d %d", creates.Load(), count)
	}
}

func TestBatchResourceFileDeliveryCommitsResponseBeforeFirstByte(t *testing.T) {
	output := `{ "custom_id":"a", "response":{"status_code":200,"body":{"id":"resp-batch","model":"gpt-5","future":1e0}} }` + "\r\n"
	_, makeContext := batchFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/files/file-input/content":
			io.WriteString(w, `{"custom_id":"a","method":"POST","url":"/v1/responses","body":{"model":"gpt-5","store":false,"background":true,"input":"x"}}`)
		case "/v1/batches":
			io.WriteString(w, `{"id":"batch-1","status":"completed","output_file_id":"file-output"}`)
		case "/v1/files/file-output/content":
			io.WriteString(w, output)
		default:
			t.Errorf("unexpected request: %s", r.URL)
			w.WriteHeader(500)
		}
	})
	c, rec := makeContext("POST", "/v1/batches", strings.NewReader(`{"input_file_id":"file-input","endpoint":"/v1/responses"}`))
	BatchRelay(c)
	if rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	owner, err := model.GetResourceOwner(context.Background(), "file", "file-output", 1)
	if err != nil {
		t.Fatal(err)
	}
	c, _ = makeContext("GET", "/v1/files/file-output/content", nil)
	c.Request.Header.Set("Range", "bytes=1-20")
	if apiErr := PrepareBatchFileDelivery(c, owner, &http.Response{StatusCode: 206, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(nil))}); apiErr == nil {
		t.Fatal("unobserved range bypassed resource observation")
	}
	c.Request.Header.Del("Range")
	response := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(output))}
	if apiErr := PrepareBatchFileDelivery(c, owner, response); apiErr != nil {
		t.Fatal(apiErr)
	}
	first := make([]byte, 1)
	if _, err = response.Body.Read(first); err != nil {
		t.Fatal(err)
	}
	responseOwner, err := model.GetResponseOwner(context.Background(), "resp-batch", 1)
	if err != nil || responseOwner.BatchOwnerID == nil {
		t.Fatalf("first byte preceded owner: %+v %v", responseOwner, err)
	}
	rest, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(append(first, rest...)) != output {
		t.Fatal("JSONL bytes changed")
	}
	// 完整 EOF 提交观察事实，重读不新建 owner。
	if err := bindBatchResultResources(context.Background(), batchTestTask(t), mustBatchData(t), []byte(output)); err != nil {
		t.Fatal(err)
	}
	c.Request.Header.Set("Range", "bytes=1-20")
	owner, err = model.GetResourceOwner(context.Background(), "file", "file-output", 1)
	if err != nil || !owner.BatchResultObserved {
		t.Fatalf("complete reader did not persist observation: %+v %v", owner, err)
	}
	if apiErr := PrepareBatchFileDelivery(c, owner, &http.Response{StatusCode: 206, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(nil))}); apiErr != nil {
		t.Fatal("observed range still requires re-observation", apiErr)
	}
}
func mustBatchData(t *testing.T) openAIBatchData {
	t.Helper()
	d, e := batchData(batchTestTask(t))
	if e != nil {
		t.Fatal(e)
	}
	return d
}

func TestBatchDeletedInputFileIsNotLeasedAndCancelIsForwarded(t *testing.T) {
	var deletes, cancels atomic.Int32
	_, makeContext := batchFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "DELETE" && r.URL.Path == "/v1/files/file-input":
			deletes.Add(1)
			io.WriteString(w, `{"id":"file-input","deleted":true}`)
		case r.URL.Path == "/v1/files/file-input/content":
			io.WriteString(w, `{"custom_id":"a","method":"POST","url":"/v1/chat/completions","body":{"model":"gpt-5"}}`)
		case r.URL.Path == "/v1/batches":
			io.WriteString(w, `{"id":"batch-1","status":"completed"}`)
		case r.URL.Path == "/v1/batches/batch-1/cancel":
			cancels.Add(1)
			w.WriteHeader(409)
			io.WriteString(w, `{"error":{"message":"already completed","type":"upstream_state"},"future":true}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
	})
	c, rec := makeContext("POST", "/v1/batches", strings.NewReader(`{"input_file_id":"file-input","endpoint":"/v1/chat/completions"}`))
	BatchRelay(c)
	if rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	c, rec = makeContext("DELETE", "/v1/files/file-input", nil)
	ResourceRelay(c)
	if rec.Code != 200 || deletes.Load() != 1 {
		t.Fatalf("delete intercepted: %d %s", rec.Code, rec.Body)
	}
	c, rec = makeContext("POST", "/v1/batches/batch-1/cancel", nil)
	BatchRelay(c)
	if rec.Code != 409 || cancels.Load() != 1 || !strings.Contains(rec.Body.String(), "already completed") {
		t.Fatalf("cancel intercepted: %d %s", rec.Code, rec.Body)
	}
}

func TestBatchKnownReferenceAndModelPermissionRejectBeforeCreate(t *testing.T) {
	for _, body := range []string{`{"model":"gpt-5","audio":{"voice":{"id":"voice-shared"}}}`, `{"model":"gpt-5","messages":[{"role":"user","content":[{"type":"file","file":{"file_id":"unowned"}}]}]}`} {
		t.Run(fmt.Sprintf("body-%d", len(body)), func(t *testing.T) {
			var creates atomic.Int32
			_, makeContext := batchFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/files/file-input/content" {
					fmt.Fprintf(w, `{"custom_id":"a","method":"POST","url":"/v1/chat/completions","body":%s}`, body)
					return
				}
				creates.Add(1)
			})
			c, rec := makeContext("POST", "/v1/batches", strings.NewReader(`{"input_file_id":"file-input","endpoint":"/v1/chat/completions"}`))
			BatchRelay(c)
			if rec.Code != 400 || creates.Load() != 0 {
				t.Fatalf("reference not rejected: %d %s create=%d", rec.Code, rec.Body, creates.Load())
			}
		})
	}
}

func TestBatchLineReaderCapacityAndOriginalBytes(t *testing.T) {
	wire := " {\"a\":1e0} \r\n\n{\"b\":2}"
	var got bytes.Buffer
	if err := readBatchLines(strings.NewReader(wire), func(line []byte) error { got.Write(line); return nil }); err != nil {
		t.Fatal(err)
	}
	if got.String() != wire {
		t.Fatal("line framing changed bytes")
	}
	if err := readBatchLines(strings.NewReader(strings.Repeat("x", batchLineBytes+1)), func([]byte) error { return nil }); err == nil {
		t.Fatal("unbounded line accepted")
	}
}

func TestBatchResultDeliveryDoesNotNeedTaskAfterCleanup(t *testing.T) {
	output := `{"custom_id":"a","response":{"status_code":200,"body":{"id":"chat-kept","model":"gpt-5","usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}}}` + "\n"
	_, makeContext := batchFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/files/file-input/content":
			io.WriteString(w, `{"custom_id":"a","method":"POST","url":"/v1/chat/completions","body":{"model":"gpt-5","store":false}}`)
		case "/v1/batches", "/v1/batches/batch-1":
			io.WriteString(w, `{"id":"batch-1","status":"completed","output_file_id":"file-output"}`)
		case "/v1/files/file-output/content":
			io.WriteString(w, output)
		default:
			t.Errorf("unexpected request: %s", r.URL)
		}
	})
	c, rec := makeContext("POST", "/v1/batches", strings.NewReader(`{"input_file_id":"file-input","endpoint":"/v1/chat/completions"}`))
	BatchRelay(c)
	if rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	task := batchTestTask(t)
	if err := pollOpenAIBatch(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	task = batchTestTask(t)
	if task.ProviderState != model.TaskProviderStateClosed {
		t.Fatal("batch not finalized")
	}
	// 模拟 90 天账务留存清理，只删 Task；资源权限和来源仍独立存在。
	if err := model.DB.Delete(task).Error; err != nil {
		t.Fatal(err)
	}
	c, rec = makeContext("GET", "/v1/batches/batch-1", nil)
	BatchRelay(c)
	if rec.Code != 200 {
		t.Fatalf("Task cleanup revoked batch access: %d %s", rec.Code, rec.Body)
	}
	c, rec = makeContext("GET", "/v1/files/file-output/content", nil)
	ResourceRelay(c)
	if rec.Code != 200 || rec.Body.String() != output {
		t.Fatalf("Task cleanup revoked result access: %d %s", rec.Code, rec.Body)
	}
	file, err := model.GetResourceOwner(context.Background(), "file", "file-output", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := authorizeRetainedBatchResult(context.Background(), file, []byte(`{"custom_id":"unknown","response":{"status_code":200,"body":{"id":"chat-unknown"}}}`)); err == nil {
		t.Fatal("cleanup allowed claiming unknown output")
	}
}

func TestBatchSafeUnknownRowsAndExactLineLimitReachEOF(t *testing.T) {
	wire := `{"custom_id":"not-observed","response":{"status_code":200,"body":{"future":{"opaque_id":"business"}}}}` + "\n"
	data := openAIBatchData{Endpoint: "/v1/responses", Items: map[string]*batchItemAdmission{}}
	if err := bindBatchResultResources(context.Background(), nil, data, []byte(wire)); err != nil {
		t.Fatal("safe unknown row was blocked", err)
	}
	full := strings.Repeat(wire, batchItemLimit)
	r := &batchOwnedResultReader{source: io.NopCloser(strings.NewReader(full)), reader: bufio.NewReader(strings.NewReader(full)), ctx: context.Background(), task: &model.Task{}, data: data}
	got, err := io.ReadAll(r)
	if err != nil || string(got) != full {
		t.Fatalf("exact limit clean EOF blocked: bytes=%d error=%v", len(got), err)
	}
}

func TestBatchResourceProjectionIgnoresUnrelatedUnionTypes(t *testing.T) {
	refs := batchResultResourceReferences("/v1/responses", []byte(`{"id":"resp-known","choices":"future-extension"}`))
	if len(refs) != 1 || refs[0].id != "resp-known" {
		t.Fatalf("unrelated field hid response id: %+v", refs)
	}
	refs = batchResultResourceReferences("/v1/chat/completions", []byte(`{"id":"chat-known","choices":[{"index":{"future":true},"message":{"audio":{"id":"audio-known"}}}]}`))
	if len(refs) != 2 || refs[0].id != "chat-known" || refs[1].id != "audio-known" || refs[1].role != "audio:unattributable" {
		t.Fatalf("choice projection hid known ids: %+v", refs)
	}
	var envelope batchEnvelope
	if err := json.Unmarshal([]byte(`{"id":"batch-known","status":{"future":true},"output_file_id":"file-known"}`), &envelope); err != nil || envelope.OutputFileID != "file-known" {
		t.Fatalf("status projection hid file id: %+v %v", envelope, err)
	}
}

func TestBatchOnlyInterpretsResourceLocationsOfItsDialect(t *testing.T) {
	for _, endpoint := range []string{"/v1/embeddings", "/v1/completions", "/v1/moderations", "/v1/chat/completions", "/v1/responses"} {
		t.Run(endpoint, func(t *testing.T) {
			var creates atomic.Int32
			body := `{"model":"gpt-5","previous_response_id":"opaque","input":{"type":"input_file","file_id":"opaque"},"conversation":"opaque","tools":[{"type":"computer","future":"opaque"}]}`
			if endpoint == "/v1/responses" {
				body = `{"model":"gpt-5","tools":[{"type":"computer","future":"opaque"}],"messages":[{"role":"user","content":[{"type":"input_file","file_id":"opaque"}]}]}`
			}
			_, makeContext := batchFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/files/file-input/content" {
					fmt.Fprintf(w, `{"custom_id":"a","method":"POST","url":%q,"body":%s}`, endpoint, body)
					return
				}
				creates.Add(1)
				io.WriteString(w, `{"id":"batch-opaque","status":"validating"}`)
			})
			c, rec := makeContext("POST", "/v1/batches", strings.NewReader(fmt.Sprintf(`{"input_file_id":"file-input","endpoint":%q}`, endpoint)))
			BatchRelay(c)
			if rec.Code != 200 || creates.Load() != 1 {
				t.Fatalf("unknown dialect fields rejected: %d %s", rec.Code, rec.Body)
			}
		})
	}
}

func TestBatchIdentityModelMappingPreservesOriginalBillingPolicy(t *testing.T) {
	channel, makeContext := batchFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/files/file-input/content":
			io.WriteString(w, `{"custom_id":"a","method":"POST","url":"/v1/chat/completions","body":{"model":"gpt-5"}}`)
		case "/v1/batches", "/v1/batches/batch-1":
			io.WriteString(w, `{"id":"batch-1","status":"completed","output_file_id":"file-output"}`)
		case "/v1/files/file-output/content":
			io.WriteString(w, `{"custom_id":"a","response":{"status_code":200,"body":{"id":"chat-original-price","model":"upstream-expensive","usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}}}`)
		default:
			t.Errorf("unexpected URL: %s", r.URL)
		}
	})
	if err := model.DB.Model(channel).Update("model_mapping", `{"gpt-5":"+gpt-5"}`).Error; err != nil {
		t.Fatal(err)
	}
	if err := model.DB.Create(&model.Price{Model: "upstream-expensive", Type: model.TokensPriceType, Input: 100, Output: 100}).Error; err != nil {
		t.Fatal(err)
	}
	if err := model.PricingInstance.Init(); err != nil {
		t.Fatal(err)
	}
	c, rec := makeContext("POST", "/v1/batches", strings.NewReader(`{"input_file_id":"file-input","endpoint":"/v1/chat/completions"}`))
	BatchRelay(c)
	if rec.Code != 200 {
		t.Fatalf("identity mapping was treated as rewrite: %d %s", rec.Code, rec.Body)
	}
	task := batchTestTask(t)
	d, err := batchData(task)
	if err != nil || !d.Items["a"].BillingOriginalModel {
		t.Fatal("lost original model policy", err)
	}
	if err := pollOpenAIBatch(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	task = batchTestTask(t)
	if task.ChargedQuota == nil || *task.ChargedQuota <= 0 || *task.ChargedQuota > 10 {
		t.Fatalf("reported model overrode original billing: %+v", task)
	}
}

func TestBatchDuplicateEvidenceDoesNotDoubleChargeOrEraseIndependentItem(t *testing.T) {
	input := `{"custom_id":"a","method":"POST","url":"/v1/chat/completions","body":{"model":"gpt-5"}}` + "\n" + `{"custom_id":"b","method":"POST","url":"/v1/chat/completions","body":{"model":"gpt-5"}}` + "\n"
	rowA := `{"custom_id":"a","response":{"status_code":200,"request_id":"request-a","body":{"id":"chat-a","model":"gpt-5","usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}}}` + "\n"
	rowB := `{"custom_id":"b","response":{"status_code":200,"request_id":"request-b","body":{"id":"chat-b","model":"gpt-5","usage":{"prompt_tokens":5,"completion_tokens":5,"total_tokens":10}}}}` + "\n"
	output := rowA + rowA + rowB + strings.ReplaceAll(rowB, `"completion_tokens":5,"total_tokens":10`, `"completion_tokens":6,"total_tokens":11`)
	_, makeContext := batchFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/files/file-input/content":
			io.WriteString(w, input)
		case "/v1/batches", "/v1/batches/batch-1":
			io.WriteString(w, `{"id":"batch-1","status":"completed","output_file_id":"file-output"}`)
		case "/v1/files/file-output/content":
			io.WriteString(w, output)
		default:
			t.Errorf("unexpected request: %s", r.URL)
		}
	})
	c, rec := makeContext("POST", "/v1/batches", strings.NewReader(`{"input_file_id":"file-input","endpoint":"/v1/chat/completions"}`))
	BatchRelay(c)
	if rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	task := batchTestTask(t)
	if err := pollOpenAIBatch(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	task = batchTestTask(t)
	d, err := batchData(task)
	if err != nil {
		t.Fatal(err)
	}
	if d.ObservedItems != 2 || d.PricedItems != 1 || d.Evidence["a"] == nil || d.Evidence["b"] != nil || task.SettlementDecision != "confirm" {
		t.Fatalf("incorrect duplicate/conflict reduction: %+v", d)
	}
}
