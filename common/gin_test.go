package common

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

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

func TestCacheRequestBodyPreservesOriginalBodyAfterReuse(t *testing.T) {
	gin.SetMode(gin.TestMode)

	originalBody := []byte(`{"model":"gpt-4o"}`)
	modifiedBody := []byte(`{"model":"gpt-4o","tools":[{"type":"function"}]}`)

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(originalBody))

	if _, err := CacheRequestBody(ctx); err != nil {
		t.Fatalf("CacheRequestBody failed: %v", err)
	}

	SetReusableRequestBody(ctx, modifiedBody)

	gotOriginal, ok := GetOriginalRequestBody(ctx)
	if !ok {
		t.Fatal("expected original request body to remain available")
	}
	if string(gotOriginal) != string(originalBody) {
		t.Fatalf("unexpected original request body: got %s want %s", gotOriginal, originalBody)
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
