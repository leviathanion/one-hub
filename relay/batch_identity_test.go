package relay

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"one-api/model"
)

func TestBatchOpaqueHandleSurvivesCreateLookupAndPoll(t *testing.T) {
	const id = " batch-raw "
	var creates, polls atomic.Int32
	_, makeContext := batchFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/files/file-input/content":
			io.WriteString(w, `{"custom_id":"a","method":"POST","url":"/v1/chat/completions","body":{"model":"gpt-5"}}`)
		case "/v1/batches":
			creates.Add(1)
			io.WriteString(w, `{"id":" batch-raw ","status":"validating"}`)
		case "/v1/batches/" + id:
			polls.Add(1)
			if r.URL.EscapedPath() != "/v1/batches/"+url.PathEscape(id) || r.URL.RawQuery != "" {
				t.Errorf("handle changed in URL: %s", r.URL)
			}
			io.WriteString(w, `{"id":" batch-raw ","status":"completed"}`)
		default:
			t.Errorf("wrong raw identifier path: %q", r.URL.Path)
			w.WriteHeader(404)
		}
	})
	c, rec := makeContext("POST", "/v1/batches", strings.NewReader(`{"input_file_id":"file-input","endpoint":"/v1/chat/completions"}`))
	BatchRelay(c)
	if rec.Code != 200 || rec.Body.String() != `{"id":" batch-raw ","status":"validating"}` {
		t.Fatalf("create identity: %d %s", rec.Code, rec.Body)
	}
	task := batchTestTask(t)
	if model.TaskProviderID(task) != id {
		t.Fatalf("Task normalized handle: %q", model.TaskProviderID(task))
	}
	owner, err := model.GetResourceOwner(context.Background(), "batch", id, 1)
	if err != nil || owner.UpstreamID == nil || *owner.UpstreamID != id {
		t.Fatalf("resource normalized handle: %+v %v", owner, err)
	}
	c, rec = makeContext("GET", "/v1/batches/batch-raw", nil)
	BatchRelay(c)
	if rec.Code != 404 || polls.Load() != 0 {
		t.Fatalf("trimmed alias reached upstream: %d %s polls=%d", rec.Code, rec.Body, polls.Load())
	}
	if err := pollOpenAIBatch(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	task = batchTestTask(t)
	if task.ProviderState != model.TaskProviderStateClosed || model.TaskProviderID(task) != id {
		t.Fatalf("poll identity/settlement: %+v", task)
	}
	c, rec = makeContext("GET", "/v1/batches/"+url.PathEscape(id), nil)
	BatchRelay(c)
	if rec.Code != 200 || rec.Body.String() != `{"id":" batch-raw ","status":"completed"}` || creates.Load() != 1 || polls.Load() != 2 {
		t.Fatalf("raw lookup: %d %s creates=%d gets=%d", rec.Code, rec.Body, creates.Load(), polls.Load())
	}
}
