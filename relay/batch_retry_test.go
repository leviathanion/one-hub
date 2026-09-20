package relay

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"one-api/model"
	"one-api/types"
)

func TestBatchTransientResultReadKeepsSlotsAcrossRestart(t *testing.T) {
	for _, clientFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "worker-retry", true: "client-first"}[clientFirst], func(t *testing.T) {
			var creates, rootReads, fileReads atomic.Int32
			var failAgain atomic.Bool
			wire := `{"custom_id":"a","response":{"status_code":200,"body":{"id":"resp-retry","model":"gpt-5","usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}}}` + "\n"
			_, makeContext := batchFixture(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/files/file-input/content":
					io.WriteString(w, `{"custom_id":"a","method":"POST","url":"/v1/responses","body":{"model":"gpt-5"}}`)
				case "/v1/batches":
					creates.Add(1)
					io.WriteString(w, `{"id":"batch-retry","status":"validating"}`)
				case "/v1/batches/batch-retry":
					if rootReads.Add(1) > 1 {
						w.WriteHeader(404)
						return
					}
					io.WriteString(w, `{"id":"batch-retry","status":"completed","output_file_id":"file-retry"}`)
				case "/v1/files/file-retry/content":
					if fileReads.Add(1) == 1 || failAgain.Load() {
						w.WriteHeader(500)
						return
					}
					io.WriteString(w, wire)
				default:
					t.Errorf("unexpected request: %s", r.URL)
				}
			})
			c, rec := makeContext("POST", "/v1/batches", strings.NewReader(`{"input_file_id":"file-input","endpoint":"/v1/responses"}`))
			BatchRelay(c)
			if rec.Code != 200 {
				t.Fatalf("create: %d %s", rec.Code, rec.Body)
			}
			task := batchTestTask(t)
			if err := pollOpenAIBatch(context.Background(), task); err != nil {
				t.Fatal(err)
			}
			// 仅使用 SQL 重载，模拟进程重启丢失所有局部观察状态。
			task = batchTestTask(t)
			if task.ProviderState != model.TaskProviderStateAccepted || task.ChargedQuota != nil {
				t.Fatalf("500 closed work: %+v", task)
			}
			slots, err := model.NewResourceOwnerRepository(model.DB).TaskSlots(context.Background(), 1, task.OwnerID)
			if err != nil {
				t.Fatal(err)
			}
			pending := false
			for _, slot := range slots {
				if slot.Kind == "response" && slot.Phase == model.ResourceOwnerReserved && slot.ReservationKind == model.ResourceReservationTaskDerived {
					pending = true
				}
			}
			if !pending {
				t.Fatal("transient failure released response capacity")
			}
			if clientFirst {
				c, rec = makeContext("GET", "/v1/files/file-retry/content", nil)
				ResourceRelay(c)
				if rec.Code != 200 || rec.Body.String() != wire {
					t.Fatalf("recovered client download: %d %s", rec.Code, rec.Body)
				}
				failAgain.Store(true) // 资源屏障已完成；不为缺费用无限延后关闭。
			}
			if err := pollOpenAIBatch(context.Background(), task); err != nil {
				t.Fatal(err)
			}
			task = batchTestTask(t)
			if task.ProviderState != model.TaskProviderStateClosed {
				t.Fatalf("recovered result did not close: %+v", task)
			}
			if creates.Load() != 1 || rootReads.Load() != 1 {
				t.Fatalf("replayed generation/root terminal: creates=%d root=%d", creates.Load(), rootReads.Load())
			}
			if _, err := model.GetResponseOwner(context.Background(), "resp-retry", 1); err != nil {
				t.Fatal("recovered result lacks owner", err)
			}
			failAgain.Store(false)
			c, rec = makeContext("GET", "/v1/files/file-retry/content", nil)
			ResourceRelay(c)
			if rec.Code != 200 || rec.Body.String() != wire {
				t.Fatalf("closed result download: %d %s", rec.Code, rec.Body)
			}
			charged := *task.ChargedQuota
			if err := pollOpenAIBatch(context.Background(), task); err != nil {
				t.Fatal(err)
			}
			if *batchTestTask(t).ChargedQuota != charged {
				t.Fatal("closed result charged twice")
			}
		})
	}
}

func TestBatchExpirySettlesCheckpointAfterPreviouslyReadFileDisappears(t *testing.T) {
	var creates atomic.Int32
	var deleted atomic.Bool
	_, makeContext := batchFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/files/file-input/content":
			io.WriteString(w, `{"custom_id":"a","method":"POST","url":"/v1/chat/completions","body":{"model":"gpt-5"}}`+"\n"+`{"custom_id":"b","method":"POST","url":"/v1/chat/completions","body":{"model":"gpt-5"}}`)
		case "/v1/batches":
			creates.Add(1)
			io.WriteString(w, `{"id":"batch-checkpoint","status":"completed","output_file_id":"file-good","error_file_id":"file-later"}`)
		case "/v1/batches/batch-checkpoint":
			io.WriteString(w, `{"id":"batch-checkpoint","status":"completed","output_file_id":"file-good","error_file_id":"file-later"}`)
		case "/v1/files/file-good":
			deleted.Store(true)
			io.WriteString(w, `{"id":"file-good","deleted":true}`)
		case "/v1/files/file-good/content":
			if deleted.Load() {
				w.WriteHeader(404)
				return
			}
			io.WriteString(w, `{"custom_id":"a","response":{"status_code":200,"body":{"id":"chat-good","model":"gpt-5","usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}}}`)
		case "/v1/files/file-later/content":
			w.WriteHeader(500)
		default:
			t.Errorf("unexpected request: %s", r.URL)
		}
	})
	c, rec := makeContext("POST", "/v1/batches", strings.NewReader(`{"input_file_id":"file-input","endpoint":"/v1/chat/completions"}`))
	BatchRelay(c)
	if rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	if err := pollOpenAIBatch(context.Background(), batchTestTask(t)); err != nil {
		t.Fatal(err)
	}
	c, rec = makeContext("DELETE", "/v1/files/file-good", nil)
	ResourceRelay(c)
	if rec.Code != 200 {
		t.Fatalf("file deletion blocked: %d %s", rec.Code, rec.Body)
	}
	if err := pollOpenAIBatch(context.Background(), batchTestTask(t)); err != nil {
		t.Fatal(err)
	}
	task := batchTestTask(t)
	d, err := batchData(task)
	if err != nil || d.PendingEvidence["a"] == nil {
		t.Fatalf("read failure erased previous evidence: %+v %v", d, err)
	}
	if task.ProviderState != model.TaskProviderStateAccepted {
		t.Fatal("second file failure closed work")
	}
	setBatchTestPrice(t, 3, 5)
	if err := model.DB.Model(task).Update("acceptance_recorded_at", time.Now().Add(-model.BatchTrackingWindow-time.Second).Unix()).Error; err != nil {
		t.Fatal(err)
	}
	if err := pollOpenAIBatch(context.Background(), batchTestTask(t)); err != nil {
		t.Fatal(err)
	}
	task = batchTestTask(t)
	if task.ProviderState != model.TaskProviderStateClosed || task.Status != model.TaskStatusUnknown || task.ChargedQuota == nil || *task.ChargedQuota != 21 {
		t.Fatalf("deadline lost/duplicated current-price evidence: %+v", task)
	}
	var reserved int64
	model.DB.Model(&model.ResourceOwner{}).Where("task_owner_id = ? AND phase = ?", task.OwnerID, model.ResourceOwnerReserved).Count(&reserved)
	if reserved != 0 || creates.Load() != 1 {
		t.Fatalf("expiry did not release local obligations, or replayed POST: slots=%d creates=%d", reserved, creates.Load())
	}
	file, err := model.GetResourceOwner(context.Background(), "file", "file-later", 1)
	if err != nil {
		t.Fatal(err)
	}
	c, _ = makeContext("GET", "/v1/files/file-later/content", nil)
	response := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"custom_id":"b","response":{"status_code":200,"body":{"id":"chat-too-late"}}}`))}
	if apiErr := PrepareBatchFileDelivery(c, file, response); apiErr != nil {
		t.Fatal(apiErr)
	}
	if got, err := io.ReadAll(response.Body); err == nil || len(got) != 0 {
		t.Fatalf("expired obligation reclaimed late ID: %q %v", got, err)
	}
	response.Body.Close()
}

func TestBatchNoInternalResourceObligationDoesNotWaitForUsage(t *testing.T) {
	_, makeContext := batchFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/files/file-input/content":
			io.WriteString(w, `{"custom_id":"a","method":"POST","url":"/v1/embeddings","body":{"model":"gpt-5","input":"x"}}`)
		case "/v1/batches", "/v1/batches/batch-no-resource":
			io.WriteString(w, `{"id":"batch-no-resource","status":"completed","output_file_id":"file-no-resource"}`)
		case "/v1/files/file-no-resource/content":
			w.WriteHeader(500)
		default:
			t.Errorf("unexpected request: %s", r.URL)
		}
	})
	c, rec := makeContext("POST", "/v1/batches", strings.NewReader(`{"input_file_id":"file-input","endpoint":"/v1/embeddings"}`))
	BatchRelay(c)
	if rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	if err := pollOpenAIBatch(context.Background(), batchTestTask(t)); err != nil {
		t.Fatal(err)
	}
	task := batchTestTask(t)
	if task.ProviderState != model.TaskProviderStateClosed || task.SettlementDecision != "cancel" {
		t.Fatalf("missing fees created a resource wait: %+v", task)
	}
}

func TestBatchPendingEvidenceBudgetKeepsPreviouslyTrustedComponents(t *testing.T) {
	d := openAIBatchData{Items: map[string]*batchItemAdmission{"a": {Model: "gpt-5"}, "b": {Model: "gpt-5"}}, Slots: map[string]uint64{}}
	u := &types.Usage{PromptTokens: 2, CompletionTokens: 3, TotalTokens: 5}
	u.MarkProviderReported()
	observed := map[string]*batchObservedUsage{"a": {digest: sha256.Sum256([]byte("a")), usage: u}, "b": {digest: sha256.Sum256([]byte("b")), usage: u}}
	if err := checkpointBatchEvidence(&d, map[string]*batchObservedUsage{"a": observed["a"]}, 1100); err != nil {
		t.Fatal(err)
	}
	if d.PendingEvidence["a"] == nil {
		t.Fatal("fixture did not retain first evidence")
	}
	if err := checkpointBatchEvidence(&d, observed, 1100); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(d)
	if err != nil || len(raw) > 1100 {
		t.Fatalf("checkpoint exceeded budget: %d %v", len(raw), err)
	}
	if !d.EvidenceCapacityLimited || d.PendingEvidence["a"] == nil {
		t.Fatalf("capacity cleared trusted component: %+v", d)
	}
	restored := restoreBatchEvidence(d)
	if restored["a"] == nil || restored["a"].usage.PromptTokens != 2 {
		t.Fatal("restart lost quantitative evidence")
	}
	if retained := batchAccountingEvidence(d, observed); len(retained) != len(d.PendingEvidence) {
		t.Fatal("overflow allowed unremembered items back into accounting")
	}
	conflict := *observed["a"]
	conflict.digest = sha256.Sum256([]byte("different"))
	mergeBatchEvidence(restored, map[string]*batchObservedUsage{"a": &conflict})
	if err := checkpointBatchEvidence(&d, restored, 1100); err != nil {
		t.Fatal(err)
	}
	if d.PendingEvidence["a"] == nil || !d.PendingEvidence["a"].Conflict {
		t.Fatal("conflict was lost under capacity pressure")
	}
}
