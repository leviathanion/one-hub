package relay

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"one-api/model"
)

func TestResourceHTTPIdempotentReplayPreservesWireResponse(t *testing.T) {
	requestBody := `{ "future": [true, {"unknown":42}], "purpose": "future-purpose" }`
	responseBody := `{ "id": "file_replayed", "future": {"unknown":true} }`
	_, calls := setupResourceHTTPTest(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != requestBody || r.Header.Get("Idempotency-Key") != "client-replay-key" {
			t.Errorf("replay request changed: body=%q key=%q err=%v", body, r.Header.Get("Idempotency-Key"), err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Location", "/v1/files/file_replayed")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, responseBody)
	})
	var ownerID uint64
	for i := 0; i < 2; i++ {
		ctx, recorder := resourceHTTPContext(http.MethodPost, "/v1/files", strings.NewReader(requestBody))
		ctx.Request.Header.Set("Content-Type", "application/json")
		ctx.Request.Header.Set("Idempotency-Key", "client-replay-key")
		ResourceRelay(ctx)
		if recorder.Code != http.StatusCreated || recorder.Body.String() != responseBody || recorder.Header().Get("Location") != "/v1/files/file_replayed" {
			t.Fatalf("request %d response changed: %d %s %v", i, recorder.Code, recorder.Body, recorder.Header())
		}
		owner, err := model.GetResourceOwner(context.Background(), "file", "file_replayed", 101)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			ownerID = owner.ID
		} else if owner.ID != ownerID {
			t.Fatalf("replay replaced owner: %d != %d", owner.ID, ownerID)
		}
	}
	var count int64
	if err := model.DB.Model(&model.ResourceOwner{}).Count(&count).Error; err != nil || count != 1 || calls.Load() != 2 {
		t.Fatalf("replay leaked owner or retried upstream: count=%d calls=%d err=%v", count, calls.Load(), err)
	}
}
