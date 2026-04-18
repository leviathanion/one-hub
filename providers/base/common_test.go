package base

import (
	"net/http"
	"net/http/httptest"
	"one-api/common/requestbody"
	"strings"
	"testing"

	"one-api/common"

	"github.com/gin-gonic/gin"
)

func TestGetRawBodyCachesRequestBodyOnDemand(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := `{"prompt":"hello world"}`

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/recraftAI/v1/styles", strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")

	provider := &BaseProvider{Context: ctx}

	gotBody, ok := provider.GetRawBody()
	if !ok {
		t.Fatal("expected GetRawBody to fall back to request body caching")
	}
	if string(gotBody) != body {
		t.Fatalf("unexpected raw body: got %q want %q", gotBody, body)
	}

	gotCanonical, ok := common.GetCanonicalRequestBody(ctx)
	if !ok {
		t.Fatal("expected canonical request body cache to be populated")
	}
	if string(gotCanonical) != body {
		t.Fatalf("unexpected canonical request body: got %q want %q", gotCanonical, body)
	}
}

type countingReadCloser struct {
	reader     *strings.Reader
	readCalls  int
	closeCalls int
}

func (r *countingReadCloser) Read(p []byte) (int, error) {
	r.readCalls++
	return r.reader.Read(p)
}

func (r *countingReadCloser) Close() error {
	r.closeCalls++
	return nil
}

func TestGetRawBodyPrefersCanonicalCacheForDecodedRequests(t *testing.T) {
	gin.SetMode(gin.TestMode)

	decodedBody := []byte(`{"prompt":"decoded body"}`)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/recraftAI/v1/styles", strings.NewReader(`{"prompt":"wire body"}`))
	ctx.Request.Header.Set("Content-Type", "application/json")

	common.SetDecodedRequestState(ctx, []byte("compressed"), decodedBody, &requestbody.DecodeMeta{
		ContentEncodings: []string{"zstd"},
		WireBytes:        len("compressed"),
		DecodedBytes:     len(decodedBody),
	})

	tracker := &countingReadCloser{reader: strings.NewReader(`{"prompt":"mutated request body"}`)}
	ctx.Request.Body = tracker

	provider := &BaseProvider{Context: ctx}
	gotBody, ok := provider.GetRawBody()
	if !ok {
		t.Fatal("expected GetRawBody to read from canonical cache")
	}
	if string(gotBody) != string(decodedBody) {
		t.Fatalf("unexpected raw body: got %q want %q", gotBody, decodedBody)
	}
	if tracker.readCalls != 0 || tracker.closeCalls != 0 {
		t.Fatalf("expected canonical cache hit to avoid rereading request body, got reads=%d closes=%d", tracker.readCalls, tracker.closeCalls)
	}
}
