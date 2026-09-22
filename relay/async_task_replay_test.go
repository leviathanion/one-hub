package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"one-api/common"
	commonresponses "one-api/common/responses"
	"one-api/model"
	providersBase "one-api/providers/base"
)

func TestBatchIdempotentCreateReplayReturnsOriginalHandle(t *testing.T) {
	const body = `{"input_file_id":"file-input","endpoint":"/v1/chat/completions","future":{"v":1e0}}`
	const response = `{"id":"batch-replayed","status":"validating","future":{"v":1e0}}`
	var creates atomic.Int32
	_, makeContext := batchFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/files/file-input/content":
			io.WriteString(w, `{"custom_id":"a","method":"POST","url":"/v1/chat/completions","body":{"model":"gpt-5","messages":[]}}`)
		case "/v1/batches":
			creates.Add(1)
			raw, _ := io.ReadAll(r.Body)
			if r.Header.Get("Idempotency-Key") != "batch-retry" || string(raw) != body {
				t.Errorf("request changed: key=%q body=%s", r.Header.Get("Idempotency-Key"), raw)
			}
			io.WriteString(w, response)
		default:
			t.Errorf("unexpected upstream work: %s %s", r.Method, r.URL)
			w.WriteHeader(500)
		}
	})
	var original *model.Task
	var initialQuota int
	var initialSlots int64
	for attempt := 0; attempt < 3; attempt++ {
		c, rec := makeContext(http.MethodPost, "/v1/batches", strings.NewReader(body))
		c.Request.Header.Set("Idempotency-Key", "batch-retry")
		BatchRelay(c)
		if rec.Code != http.StatusOK || rec.Body.String() != response {
			t.Fatalf("attempt %d: status=%d body=%s", attempt, rec.Code, rec.Body)
		}
		owner, err := model.GetResourceOwner(context.Background(), "batch", "batch-replayed", 1)
		if err != nil || owner.TaskOwnerID == nil {
			t.Fatalf("missing batch handle: %+v %v", owner, err)
		}
		var user model.User
		var slots int64
		if err := model.DB.First(&user, 1).Error; err != nil {
			t.Fatal(err)
		}
		if err := model.DB.Model(&model.ResourceOwner{}).Count(&slots).Error; err != nil {
			t.Fatal(err)
		}
		if attempt == 0 {
			original = batchTestTask(t)
			initialQuota, initialSlots = user.Quota, slots
		} else if *owner.TaskOwnerID != original.OwnerID || user.Quota != initialQuota || slots != initialSlots {
			t.Fatalf("replay changed owner/reservations: owner=%s quota=%d slots=%d", *owner.TaskOwnerID, user.Quota, slots)
		}
	}
	assertSingleAcceptedReplayTask(t, model.TaskPlatformOpenAIBatch, original.OwnerID, 2)
	if creates.Load() != 3 {
		t.Fatalf("unexpected automatic retry: creates=%d", creates.Load())
	}
}

func TestBackgroundIdempotentCreateReplayAccountsOriginalTask(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			body := fmt.Sprintf(`{"model":"gpt-5","background":true,"stream":%t,"store":false,"input":"hello","future":{"v":1e0}}`, stream)
			var creates atomic.Int32
			var wantWire atomic.Value
			first, _ := backgroundFixture(t, func(w http.ResponseWriter, r *http.Request) {
				attempt := creates.Add(1)
				raw, _ := io.ReadAll(r.Body)
				if r.Method != http.MethodPost || r.Header.Get("Idempotency-Key") != "response-retry" || string(raw) != body {
					t.Errorf("unexpected upstream work: method=%s key=%q body=%s", r.Method, r.Header.Get("Idempotency-Key"), raw)
				}
				response := `{"id":"resp-replayed","status":"queued","future":{"v":1e0}}`
				event := "response.created"
				if attempt > 1 {
					response = `{"id":"resp-replayed","status":"completed","model":"gpt-5","usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5},"future":{"v":1e0}}`
					event = "response.completed"
				}
				wire := response
				w.Header().Set("Content-Type", "application/json")
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					wire = fmt.Sprintf("data: {\"type\":%q,\"response\":%s}\n\n", event, response)
				}
				wantWire.Store(wire)
				io.WriteString(w, wire)
			}, body)
			model.PricingInstance.Prices["gpt-5"] = &model.Price{Model: "gpt-5", Type: model.TokensPriceType, Input: 1, Output: 1}
			var ownerID string
			var charged int64
			for attempt := 0; attempt < 3; attempt++ {
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				for key, value := range first.c.Keys {
					c.Set(key, value)
				}
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
				c.Request.Header.Set("Idempotency-Key", "response-retry")
				enableResponsesTestDeadline(c)
				common.SetReusableRequestBody(c, []byte(body))
				envelope, err := commonresponses.ParseRawEnvelope([]byte(body))
				if err != nil {
					t.Fatal(err)
				}
				provider, mapped, err := prepareProviderForChannel(c, "gpt-5", first.provider.GetChannel())
				if err != nil {
					t.Fatal(err)
				}
				r := NewRelayResponses(c)
				r.provider, r.modelName, r.originalModel = provider, mapped, "gpt-5"
				r.rawEnvelope, r.responsesRequest = envelope, envelope.Projection
				r.selectedDataPath = providersBase.DataPathExactWire
				apiErr, done := RelayHandler(r)
				waitBackgroundObserver(t, c)
				wireMatches := rec.Body.String() == wantWire.Load()
				if !stream {
					var got, want map[string]json.RawMessage
					wireMatches = json.Unmarshal(rec.Body.Bytes(), &got) == nil && json.Unmarshal([]byte(wantWire.Load().(string)), &want) == nil
					for _, key := range []string{"id", "status", "future"} {
						wireMatches = wireMatches && string(got[key]) == string(want[key])
					}
				}
				if apiErr != nil || !done || rec.Code != http.StatusOK || !wireMatches {
					t.Fatalf("attempt %d: err=%+v done=%t status=%d body=%s", attempt, apiErr, done, rec.Code, rec.Body)
				}
				owner, err := model.GetResponseOwner(context.Background(), "resp-replayed", 1)
				if err != nil || owner.TaskOwnerID == nil {
					t.Fatalf("missing response owner: %+v %v", owner, err)
				}
				if attempt == 0 {
					ownerID = *owner.TaskOwnerID
				} else if *owner.TaskOwnerID != ownerID {
					t.Fatal("replay replaced response owner")
				}
				task, err := model.GetBackgroundResponseTask(context.Background(), ownerID)
				if err != nil {
					t.Fatal(err)
				}
				if attempt > 0 {
					if task.ProviderState != model.TaskProviderStateClosed || task.ChargedQuota == nil || *task.ChargedQuota <= 0 {
						t.Fatalf("replayed evidence did not settle original task: %+v", task)
					}
					if attempt == 1 {
						charged = *task.ChargedQuota
					} else if *task.ChargedQuota != charged {
						t.Fatal("replay changed settlement")
					}
					var user model.User
					if err := model.DB.First(&user, 1).Error; err != nil {
						t.Fatal(err)
					}
					if user.Quota != 100000-int(charged) || user.UsedQuota != int(charged) {
						t.Fatalf("replay changed balance: quota=%d used=%d charged=%d", user.Quota, user.UsedQuota, charged)
					}
				}
			}
			assertSingleAcceptedReplayTask(t, model.TaskPlatformOpenAIResponsesBackground, ownerID, 2)
			if creates.Load() != 3 {
				t.Fatalf("unexpected automatic retry: creates=%d", creates.Load())
			}
		})
	}
}

func assertSingleAcceptedReplayTask(t *testing.T, platform, ownerID string, duplicates int) {
	t.Helper()
	var tasks []model.Task
	if err := model.DB.Where("platform = ?", platform).Find(&tasks).Error; err != nil {
		t.Fatal(err)
	}
	if len(tasks) != duplicates+1 {
		t.Fatalf("unexpected task count: %d", len(tasks))
	}
	for _, task := range tasks {
		if task.OwnerID == ownerID {
			if task.AcceptanceRecordedAt == nil {
				t.Fatal("original task lost acceptance")
			}
			continue
		}
		if task.ProviderState != model.TaskProviderStateClosed || task.AcceptanceRecordedAt != nil || task.ChargedQuota == nil || *task.ChargedQuota != 0 || task.SettlementDecision != "cancel" {
			t.Fatalf("duplicate reservation not released: %+v", task)
		}
	}
}
