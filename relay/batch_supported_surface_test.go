package relay

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"one-api/model"
)

func TestBatchResponsesSavedPromptRejectsBeforeCreateAndReservation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		prompt string
	}{
		{name: "saved_prompt", prompt: `{"id":"pmpt_shared"}`},
		{name: "saved_prompt_with_file_variable", prompt: `{"id":"pmpt_shared","variables":{"document":{"type":"input_file","file_id":"file-unowned"}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var creates atomic.Int32
			_, makeContext := batchFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/v1/files/file-input/content":
					fmt.Fprintf(w, `{"custom_id":"a","method":"POST","url":"/v1/responses","body":{"model":"gpt-5","prompt":%s}}`, tc.prompt)
				case "/v1/batches":
					creates.Add(1)
					io.WriteString(w, `{"id":"batch-prompt","status":"validating"}`)
				default:
					t.Errorf("意外的上游请求：%s %s", r.Method, r.URL)
					w.WriteHeader(http.StatusInternalServerError)
				}
			})
			userBefore, tokenBefore := readResponsesWSQuotaFixture(t)
			c, rec := makeContext("POST", "/v1/batches", strings.NewReader(`{"input_file_id":"file-input","endpoint":"/v1/responses"}`))
			BatchRelay(c)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"code":"batch_admission_failed"`) || !strings.Contains(rec.Body.String(), "saved prompt") {
				t.Errorf("saved prompt 应在准入时拒绝：%d %s", rec.Code, rec.Body)
			}
			var tasks int64
			if err := model.DB.Model(&model.Task{}).Count(&tasks).Error; err != nil {
				t.Fatal(err)
			}
			if creates.Load() != 0 || tasks != 0 {
				t.Errorf("准入失败后产生了上游提交或本地任务：creates=%d tasks=%d", creates.Load(), tasks)
			}
			userAfter, tokenAfter := readResponsesWSQuotaFixture(t)
			if userAfter.Quota != userBefore.Quota || userAfter.UsedQuota != userBefore.UsedQuota || tokenAfter.RemainQuota != tokenBefore.RemainQuota || tokenAfter.UsedQuota != tokenBefore.UsedQuota {
				t.Errorf("准入失败改变了额度：user=%d/%d -> %d/%d token=%d/%d -> %d/%d", userBefore.Quota, userBefore.UsedQuota, userAfter.Quota, userAfter.UsedQuota, tokenBefore.RemainQuota, tokenBefore.UsedQuota, tokenAfter.RemainQuota, tokenAfter.UsedQuota)
			}
		})
	}
}

func TestBatchResponsesInlinePromptPreservesUpstreamResponseWithoutRetry(t *testing.T) {
	for _, tc := range []struct {
		name        string
		extraFields string
		input       string
		status      int
		body        string
	}{
		{name: "accepted", status: http.StatusOK, body: `{ "id":"batch-inline", "status":"validating", "future":1e0 }`},
		{name: "null_prompt", extraFields: `,"prompt":null`, status: http.StatusOK, body: `{"id":"batch-inline","status":"validating"}`},
		{name: "empty_prompt", extraFields: `,"prompt":{}`, status: http.StatusOK, body: `{"id":"batch-inline","status":"validating"}`},
		{name: "future_input_union", input: `{"type":"future_input","file_id":"business-value"}`, status: http.StatusOK, body: `{"id":"batch-inline","status":"validating"}`},
		{name: "upstream_error", status: http.StatusServiceUnavailable, body: `{ "error":{"message":"try later","type":"server_error"}, "future":1e0 }`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			post := `{ "input_file_id":"file-input", "endpoint":"/v1/responses", "future":1e0 }`
			input := tc.input
			if input == "" {
				input = `"你好"`
			}
			var creates atomic.Int32
			_, makeContext := batchFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/v1/files/file-input/content":
					fmt.Fprintf(w, `{"custom_id":"a","method":"POST","url":"/v1/responses","body":{"model":"gpt-5","instructions":"请回答用户问题","input":%s,"future":{"file_id":"business-value"}%s}}`, input, tc.extraFields)
				case "/v1/batches":
					creates.Add(1)
					raw, err := io.ReadAll(r.Body)
					if err != nil || string(raw) != post {
						t.Errorf("Batch 创建请求发生改写：%s error=%v", raw, err)
					}
					w.WriteHeader(tc.status)
					io.WriteString(w, tc.body)
				default:
					t.Errorf("意外的上游请求：%s %s", r.Method, r.URL)
					w.WriteHeader(http.StatusInternalServerError)
				}
			})
			c, rec := makeContext("POST", "/v1/batches", strings.NewReader(post))
			BatchRelay(c)
			if rec.Code != tc.status || rec.Body.String() != tc.body || creates.Load() != 1 {
				t.Fatalf("内联内容应准入并保留单次上游响应：status=%d body=%s creates=%d", rec.Code, rec.Body, creates.Load())
			}
		})
	}
}
