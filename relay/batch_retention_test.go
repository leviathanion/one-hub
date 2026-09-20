package relay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"one-api/model"
)

func batchRetentionFixture(t *testing.T, endpoint, inputBody, output string) (*model.Task, func(string, string, io.Reader) (*gin.Context, *httptest.ResponseRecorder)) {
	t.Helper()
	_, makeContext := batchFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/files/file-input/content":
			fmt.Fprintf(w, `{"custom_id":"a","method":"POST","url":%q,"body":%s}`, endpoint, inputBody)
		case "/v1/batches", "/v1/batches/batch-retained":
			io.WriteString(w, `{"id":"batch-retained","status":"completed","output_file_id":"file-retained"}`)
		case "/v1/files/file-retained/content":
			io.WriteString(w, output)
		default:
			t.Errorf("unexpected request: %s", r.URL)
		}
	})
	c, rec := makeContext("POST", "/v1/batches", strings.NewReader(fmt.Sprintf(`{"input_file_id":"file-input","endpoint":%q}`, endpoint)))
	BatchRelay(c)
	if rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	return batchTestTask(t), makeContext
}

func TestBatchObservedFileSurvivesActualChildAndTaskRetentionCleanup(t *testing.T) {
	for _, tc := range []struct{ name, endpoint, input, output, childKind, childID string }{
		{"response", "/v1/responses", `{"model":"gpt-5","input":"x"}`, `{"custom_id":"a","response":{"status_code":200,"body":{"id":"resp-retained","model":"gpt-5","usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}}}` + "\n", "response", "resp-retained"},
		{"audio", "/v1/chat/completions", `{"model":"gpt-5","audio":{"voice":"alloy","format":"wav"}}`, `{"custom_id":"a","response":{"status_code":200,"body":{"id":"chat-retained","model":"gpt-5","choices":[{"index":0,"message":{"audio":{"id":"audio-retained","expires_at":1}}}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}}}` + "\n", "chat_audio", "audio-retained"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task, makeContext := batchRetentionFixture(t, tc.endpoint, tc.input, tc.output)
			if err := pollOpenAIBatch(context.Background(), task); err != nil {
				t.Fatal(err)
			}
			file, err := model.GetResourceOwner(context.Background(), "file", "file-retained", 1)
			if err != nil || !file.BatchResultObserved {
				t.Fatalf("complete scan marker=%+v error=%v", file, err)
			}
			// 推进真实保留窗口并执行正式清理：Response 37 天、audio 7 天、
			// settled Task 90 天。此处不保留新鲜子 owner 冒充老数据。
			future := time.Now().Add(model.BatchTaskRetention + 24*time.Hour)
			if _, err := model.DeleteExpiredResponseOwners(context.Background(), future); err != nil {
				t.Fatal(err)
			}
			if _, err := model.DeleteExpiredResourceOwners(context.Background(), future); err != nil {
				t.Fatal(err)
			}
			if n, err := model.DeleteExpiredOpenAIBatchTasks(context.Background(), future); err != nil || n != 1 {
				t.Fatalf("task cleanup=%d %v", n, err)
			}
			assertBatchRetainedChildAbsent(t, tc.childKind, tc.childID)
			if _, err := model.GetOpenAIBatchTask(context.Background(), task.OwnerID); !errors.Is(err, gorm.ErrRecordNotFound) {
				t.Fatalf("Task survived cleanup: %v", err)
			}
			for attempt := 0; attempt < 2; attempt++ {
				c, rec := makeContext("GET", "/v1/batches/batch-retained", nil)
				BatchRelay(c)
				if rec.Code != 200 {
					t.Fatalf("retained Batch blocked: %d %s", rec.Code, rec.Body)
				}
				c, rec = makeContext("GET", "/v1/files/file-retained/content", nil)
				ResourceRelay(c)
				if rec.Code != 200 || rec.Body.String() != tc.output {
					t.Fatalf("retained File blocked/rewritten: %d %s", rec.Code, rec.Body)
				}
			}
			assertBatchRetainedChildAbsent(t, tc.childKind, tc.childID)
			// 已完整观察的同一不可变文件不需要再通过全文件扫描才能 Range。
			c, _ := makeContext("GET", "/v1/files/file-retained/content", nil)
			c.Request.Header.Set("Range", "bytes=0-3")
			partial := &http.Response{StatusCode: http.StatusPartialContent, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("part"))}
			if apiErr := PrepareBatchFileDelivery(c, file, partial); apiErr != nil {
				t.Fatal("observed File range still depends on expired children", apiErr)
			}
			partial.Body.Close()
		})
	}
}

func assertBatchRetainedChildAbsent(t *testing.T, kind, id string) {
	t.Helper()
	if kind == "response" {
		if _, err := model.GetResponseOwner(context.Background(), id, 1); !errors.Is(err, model.ErrResponseOwnerNotFound) {
			t.Fatalf("response owner was retained/recreated: %v", err)
		}
		return
	}
	if _, err := model.GetResourceOwner(context.Background(), kind, id, 1); !errors.Is(err, model.ErrResourceOwnerNotFound) {
		t.Fatalf("resource owner was retained/recreated: %v", err)
	}
}

func TestBatchIncompleteResultDoesNotAcquireCompletedObservation(t *testing.T) {
	wire := `{"custom_id":"a","response":{"status_code":200,"body":{"id":"resp-incomplete"}}}` + "\n"
	task, makeContext := batchRetentionFixture(t, "/v1/responses", `{"model":"gpt-5","input":"x"}`, wire)
	file, err := model.GetResourceOwner(context.Background(), "file", "file-retained", 1)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := makeContext("GET", "/v1/files/file-retained/content", nil)
	response := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(io.MultiReader(strings.NewReader(wire), batchRetentionReadError{}))}
	if apiErr := PrepareBatchFileDelivery(c, file, response); apiErr != nil {
		t.Fatal(apiErr)
	}
	if _, err := io.ReadAll(response.Body); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("transport failure hidden: %v", err)
	}
	response.Body.Close()
	file, err = model.GetResourceOwner(context.Background(), "file", "file-retained", 1)
	if err != nil || file.BatchResultObserved {
		t.Fatalf("truncated scan marked complete: %+v %v", file, err)
	}
	task.Status = model.TaskStatusUnknown
	if _, err := model.FinalizeOpenAIBatchBillingOwner(context.Background(), task, 0, "cancel", task.Data); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(model.BatchTaskRetention + 24*time.Hour)
	if _, err := model.DeleteExpiredResponseOwners(context.Background(), future); err != nil {
		t.Fatal(err)
	}
	if _, err := model.DeleteExpiredOpenAIBatchTasks(context.Background(), future); err != nil {
		t.Fatal(err)
	}
	response = &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(wire))}
	if apiErr := PrepareBatchFileDelivery(c, file, response); apiErr != nil {
		t.Fatal(apiErr)
	}
	got, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err == nil || len(got) != 0 {
		t.Fatalf("unobserved File bypassed missing first-delivery proof: %q %v", got, err)
	}
	assertBatchRetainedChildAbsent(t, "response", "resp-incomplete")
}

type batchRetentionReadError struct{}

func (batchRetentionReadError) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestBatchRootGetDoesNotDependOnDeletedOutputOwnerRetention(t *testing.T) {
	wire := `{"custom_id":"a","response":{"status_code":200,"body":{"id":"resp-root"}}}` + "\n"
	task, makeContext := batchRetentionFixture(t, "/v1/responses", `{"model":"gpt-5","input":"x"}`, wire)
	if err := pollOpenAIBatch(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if err := model.ObserveResourceOwnerDelete(context.Background(), "file", "file-retained", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := model.DeleteExpiredResourceOwners(context.Background(), time.Now().Add(8*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	assertBatchRetainedChildAbsent(t, "file", "file-retained")
	// Task 尚在时走 observeBatchFiles；90 天清理后走 root owner 来源事实。
	for _, taskExpired := range []bool{false, true} {
		if taskExpired {
			if _, err := model.DeleteExpiredOpenAIBatchTasks(context.Background(), time.Now().Add(model.BatchTaskRetention+24*time.Hour)); err != nil {
				t.Fatal(err)
			}
		}
		c, rec := makeContext("GET", "/v1/batches/batch-retained", nil)
		BatchRelay(c)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"output_file_id":"file-retained"`) {
			t.Fatalf("root GET gated on deleted child (task expired=%v): %d %s", taskExpired, rec.Code, rec.Body)
		}
		assertBatchRetainedChildAbsent(t, "file", "file-retained")
	}
}

func TestBatchPollUnknownResourceCannotMarkFileObserved(t *testing.T) {
	wire := `{"custom_id":"not-admitted","response":{"status_code":200,"body":{"id":"resp-not-admitted"}}}` + "\n"
	task, _ := batchRetentionFixture(t, "/v1/responses", `{"model":"gpt-5","input":"x"}`, wire)
	if err := pollOpenAIBatch(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	file, err := model.GetResourceOwner(context.Background(), "file", "file-retained", 1)
	if err != nil || file.BatchResultObserved {
		t.Fatalf("skipped accounting item bypassed ownership marker: %+v %v", file, err)
	}
	assertBatchRetainedChildAbsent(t, "response", "resp-not-admitted")
}
