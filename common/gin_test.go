package common

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"one-api/common/requestbody"

	"github.com/gin-gonic/gin"
)

func TestCacheRequestBodyAndCloneReusableBodyMap(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"gpt-4o","nested":{"value":1},"items":[{"k":"v"}]}`)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))

	cachedBody, err := CacheRequestBody(ctx)
	if err != nil {
		t.Fatalf("expected body caching to succeed, got %v", err)
	}
	if string(cachedBody) != string(body) {
		t.Fatalf("expected cached body to match original body")
	}

	firstMap, err := CloneReusableBodyMap(ctx)
	if err != nil {
		t.Fatalf("expected cached body map to be available, got %v", err)
	}
	secondMap, err := CloneReusableBodyMap(ctx)
	if err != nil {
		t.Fatalf("expected cloned body map to be reusable, got %v", err)
	}

	firstMap["model"] = "changed"
	nested := firstMap["nested"].(map[string]interface{})
	nested["value"] = float64(9)
	items := firstMap["items"].([]interface{})
	items[0].(map[string]interface{})["k"] = "changed"

	if secondMap["model"] != "gpt-4o" {
		t.Fatalf("expected cloned maps to be isolated, got %v", secondMap["model"])
	}
	if secondMap["nested"].(map[string]interface{})["value"] != float64(1) {
		t.Fatalf("expected nested map clone to remain unchanged, got %v", secondMap["nested"])
	}
	if secondMap["items"].([]interface{})[0].(map[string]interface{})["k"] != "v" {
		t.Fatalf("expected array clone to remain unchanged, got %v", secondMap["items"])
	}
}

func TestSetReusableRequestBodyMapUsesProvidedMap(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"gpt-4o","nested":{"value":1}}`)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))

	requestMap := map[string]interface{}{
		"model": "gpt-4o",
		"nested": map[string]interface{}{
			"value": float64(1),
		},
	}

	SetReusableRequestBodyMap(ctx, body, requestMap)

	gotMap, err := GetReusableBodyMap(ctx)
	if err != nil {
		t.Fatalf("expected cached body map to be returned, got %v", err)
	}
	if gotMap["model"] != "gpt-4o" {
		t.Fatalf("expected provided map to be reused, got %v", gotMap["model"])
	}

	requestMap["model"] = "changed"
	if gotMap["model"] != "changed" {
		t.Fatalf("expected stored map reference to match provided map, got %v", gotMap["model"])
	}
}

func TestCacheRequestBodyTracksCanonicalBodyAfterReuse(t *testing.T) {
	gin.SetMode(gin.TestMode)

	initialBody := []byte(`{"model":"gpt-4o"}`)
	modifiedBody := []byte(`{"model":"gpt-4o","tools":[{"type":"function"}]}`)

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(initialBody))

	if _, err := CacheRequestBody(ctx); err != nil {
		t.Fatalf("CacheRequestBody failed: %v", err)
	}

	SetReusableRequestBody(ctx, modifiedBody)

	gotCanonical, ok := GetCanonicalRequestBody(ctx)
	if !ok {
		t.Fatal("expected canonical request body to remain available")
	}
	if string(gotCanonical) != string(modifiedBody) {
		t.Fatalf("unexpected canonical request body: got %s want %s", gotCanonical, modifiedBody)
	}

	gotOriginal, ok := GetOriginalRequestBody(ctx)
	if !ok {
		t.Fatal("expected original request body to remain available")
	}
	if string(gotOriginal) != string(initialBody) {
		t.Fatalf("unexpected original request body: got %s want %s", gotOriginal, initialBody)
	}
}

func TestCacheRequestBodyHandlesNilRequestBody(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	req, err := http.NewRequest(http.MethodPost, "/", nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	req.Body = nil
	ctx.Request = req

	cachedBody, err := CacheRequestBody(ctx)
	if err != nil {
		t.Fatalf("expected nil request body to be handled, got %v", err)
	}
	if len(cachedBody) != 0 {
		t.Fatalf("expected empty cached body, got %q", cachedBody)
	}

	gotMap, err := GetReusableBodyMap(ctx)
	if err != nil {
		t.Fatalf("expected nil request body map lookup to succeed, got %v", err)
	}
	if gotMap != nil {
		t.Fatalf("expected no request body map, got %v", gotMap)
	}

	gotOriginal, ok := GetOriginalRequestBody(ctx)
	if !ok {
		t.Fatal("expected original request body cache to be set")
	}
	if len(gotOriginal) != 0 {
		t.Fatalf("expected empty original request body, got %q", gotOriginal)
	}
}

func TestSetRequestBodyMetadataDoesNotOverwriteUnreadRequestBody(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"gpt-4o","stream":true}`)

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))

	SetRequestBodyDecodeMeta(ctx, &requestbody.DecodeMeta{DecodedBytes: len(body)})
	SetRequestBodyReparseNeeded(ctx, true)

	cachedBody, err := CacheRequestBody(ctx)
	if err != nil {
		t.Fatalf("expected body caching to succeed after metadata updates, got %v", err)
	}
	if string(cachedBody) != string(body) {
		t.Fatalf("expected unread request body to remain intact, got %q", cachedBody)
	}
	if !GetRequestBodyReparseNeeded(ctx) {
		t.Fatal("expected reparse flag to remain set")
	}
}

func TestSetDecodedRequestStateTracksWireBodyAndResetsBodyMap(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte(`{"wire":true}`)))

	SetReusableRequestBodyMap(ctx, []byte(`{"wire":true}`), map[string]interface{}{
		"wire": true,
	})

	wireBody := []byte("compressed-payload")
	decodedBody := []byte(`{"model":"gpt-5"}`)
	meta := &requestbody.DecodeMeta{
		ContentEncodings: []string{"zstd"},
		WireBytes:        len(wireBody),
		DecodedBytes:     len(decodedBody),
	}

	SetDecodedRequestState(ctx, wireBody, decodedBody, meta)

	gotCanonical, ok := GetCanonicalRequestBody(ctx)
	if !ok || string(gotCanonical) != string(decodedBody) {
		t.Fatalf("unexpected canonical request body: got %q ok=%v", gotCanonical, ok)
	}

	gotOriginal, ok := GetOriginalRequestBody(ctx)
	if !ok || string(gotOriginal) != string(decodedBody) {
		t.Fatalf("unexpected original request body: got %q ok=%v", gotOriginal, ok)
	}

	gotWire, ok := GetWireRequestBody(ctx)
	if !ok || string(gotWire) != string(wireBody) {
		t.Fatalf("unexpected wire request body: got %q ok=%v", gotWire, ok)
	}

	gotMeta, ok := GetRequestBodyDecodeMeta(ctx)
	if !ok || gotMeta.DecodedBytes != len(decodedBody) {
		t.Fatalf("unexpected decode metadata: got %+v ok=%v", gotMeta, ok)
	}

	gotMap, err := GetReusableBodyMap(ctx)
	if err != nil {
		t.Fatalf("expected decoded body map to be rebuilt, got %v", err)
	}
	if gotMap["model"] != "gpt-5" {
		t.Fatalf("expected decoded body map, got %+v", gotMap)
	}
	if _, exists := gotMap["wire"]; exists {
		t.Fatalf("expected stale body map to be cleared, got %+v", gotMap)
	}

	bodyBytes, err := io.ReadAll(ctx.Request.Body)
	if err != nil {
		t.Fatalf("expected reusable request body, got %v", err)
	}
	if string(bodyBytes) != string(decodedBody) {
		t.Fatalf("unexpected request body after decode state update: got %q want %q", bodyBytes, decodedBody)
	}
}
