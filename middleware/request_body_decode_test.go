package middleware

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"one-api/common"
	"one-api/common/config"
	"one-api/common/logger"
	"one-api/common/surface"
	provider "one-api/providers/midjourney"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"go.uber.org/zap"
)

func TestNormalizeEncodedRequestBodySupportsReusableBodyBinding(t *testing.T) {
	restoreConfig := overrideRequestBodyDecodeConfig(t, true, 1<<20, 1<<20, 64, 2)
	defer restoreConfig()

	engine := newDecodeTestEngine()
	engine.POST("/bind", func(c *gin.Context) {
		var req struct {
			Model string `json:"model" binding:"required"`
		}
		if err := common.UnmarshalBodyReusable(c, &req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		canonicalBody, _ := common.GetCanonicalRequestBody(c)
		wireBody, hasWireBody := common.GetWireRequestBody(c)
		meta, _ := common.GetRequestBodyDecodeMeta(c)
		c.JSON(http.StatusOK, gin.H{
			"model":            req.Model,
			"canonical_body":   string(canonicalBody),
			"has_wire_body":    hasWireBody,
			"wire_body_len":    len(wireBody),
			"decoded_bytes":    meta.DecodedBytes,
			"encodings":        meta.ContentEncodings,
			"content_encoding": c.Request.Header.Get("Content-Encoding"),
			"content_length":   c.Request.ContentLength,
		})
	})

	plain := []byte(`{"model":"gpt-5"}`)
	resp := performCompressedRequest(t, engine, http.MethodPost, "/bind", "application/json", "zstd", plain)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected bind route to succeed, got status=%d body=%s", resp.Code, resp.Body.String())
	}

	var payload struct {
		Model           string   `json:"model"`
		CanonicalBody   string   `json:"canonical_body"`
		HasWireBody     bool     `json:"has_wire_body"`
		WireBodyLen     int      `json:"wire_body_len"`
		DecodedBytes    int      `json:"decoded_bytes"`
		Encodings       []string `json:"encodings"`
		ContentEncoding string   `json:"content_encoding"`
		ContentLength   int64    `json:"content_length"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
		t.Fatalf("expected valid json payload, got %v", err)
	}
	if payload.Model != "gpt-5" {
		t.Fatalf("expected model gpt-5, got %q", payload.Model)
	}
	if payload.CanonicalBody != string(plain) {
		t.Fatalf("expected canonical body to expose decoded payload, got %q", payload.CanonicalBody)
	}
	if !payload.HasWireBody || payload.WireBodyLen == 0 {
		t.Fatalf("expected wire body to be retained for diagnostics, got has_wire_body=%v wire_body_len=%d", payload.HasWireBody, payload.WireBodyLen)
	}
	if payload.DecodedBytes != len(plain) {
		t.Fatalf("expected decoded bytes %d, got %d", len(plain), payload.DecodedBytes)
	}
	if len(payload.Encodings) != 1 || payload.Encodings[0] != "zstd" {
		t.Fatalf("unexpected encodings payload: %+v", payload.Encodings)
	}
	if payload.ContentEncoding != "" {
		t.Fatalf("expected content-encoding to be cleared, got %q", payload.ContentEncoding)
	}
	if payload.ContentLength != int64(len(plain)) {
		t.Fatalf("expected content length to be rewritten to %d, got %d", len(plain), payload.ContentLength)
	}
}

func TestNormalizeEncodedRequestBodySupportsJSONDecoder(t *testing.T) {
	restoreConfig := overrideRequestBodyDecodeConfig(t, true, 1<<20, 1<<20, 64, 2)
	defer restoreConfig()

	engine := newDecodeTestEngine()
	engine.POST("/decode", func(c *gin.Context) {
		normalizedBody, err := common.CacheRequestBody(c)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		var payload map[string]any
		if err := json.NewDecoder(c.Request.Body).Decode(&payload); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"model":           payload["model"],
			"normalized_body": string(normalizedBody),
		})
	})

	plain := []byte(`{"model":"gpt-5.1"}`)
	resp := performCompressedRequest(t, engine, http.MethodPost, "/decode", "application/json", "zstd", plain)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected decoder route to succeed, got status=%d body=%s", resp.Code, resp.Body.String())
	}

	var payload map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
		t.Fatalf("expected valid json payload, got %v", err)
	}
	if payload["model"] != "gpt-5.1" {
		t.Fatalf("expected model gpt-5.1, got %#v", payload["model"])
	}
	if payload["normalized_body"] != string(plain) {
		t.Fatalf("expected normalized body to remain available before decode, got %#v", payload["normalized_body"])
	}
}

func TestNormalizeEncodedRequestBodySupportsRepeatedContentEncodingHeaders(t *testing.T) {
	restoreConfig := overrideRequestBodyDecodeConfig(t, true, 1<<20, 1<<20, 64, 2)
	defer restoreConfig()

	engine := newDecodeTestEngine()
	engine.POST("/bind", func(c *gin.Context) {
		var req struct {
			Model string `json:"model" binding:"required"`
		}
		if err := common.UnmarshalBodyReusable(c, &req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		meta, _ := common.GetRequestBodyDecodeMeta(c)
		c.JSON(http.StatusOK, gin.H{
			"model":     req.Model,
			"encodings": meta.ContentEncodings,
		})
	})

	plain := []byte(`{"model":"gpt-5-double"}`)
	compressedTwice := mustCompressRequestBody(t, mustCompressRequestBody(t, plain))
	req := httptest.NewRequest(http.MethodPost, "/bind", bytes.NewReader(compressedTwice))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Add("Content-Encoding", "zstd")
	req.Header.Add("Content-Encoding", "zstd")

	resp := httptest.NewRecorder()
	engine.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("expected repeated content-encoding headers to decode successfully, got status=%d body=%s", resp.Code, resp.Body.String())
	}

	var payload struct {
		Model     string   `json:"model"`
		Encodings []string `json:"encodings"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
		t.Fatalf("expected valid json payload, got %v", err)
	}
	if payload.Model != "gpt-5-double" {
		t.Fatalf("expected model gpt-5-double, got %q", payload.Model)
	}
	if len(payload.Encodings) != 2 || payload.Encodings[0] != "zstd" || payload.Encodings[1] != "zstd" {
		t.Fatalf("unexpected content encodings: %+v", payload.Encodings)
	}
}

func TestNormalizeEncodedRequestBodySupportsMultipart(t *testing.T) {
	restoreConfig := overrideRequestBodyDecodeConfig(t, true, 1<<20, 1<<20, 64, 2)
	defer restoreConfig()

	engine := newDecodeTestEngine()
	engine.POST("/multipart", func(c *gin.Context) {
		if err := c.Request.ParseMultipartForm(1 << 20); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		file, _, err := c.Request.FormFile("file")
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		defer file.Close()

		content, err := io.ReadAll(file)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"name": c.Request.FormValue("name"),
			"file": string(content),
		})
	})

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("name", "codex"); err != nil {
		t.Fatalf("failed to write multipart field: %v", err)
	}
	part, err := writer.CreateFormFile("file", "payload.txt")
	if err != nil {
		t.Fatalf("failed to create multipart file: %v", err)
	}
	if _, err = part.Write([]byte("hello-multipart")); err != nil {
		t.Fatalf("failed to write multipart file: %v", err)
	}
	if err = writer.Close(); err != nil {
		t.Fatalf("failed to close multipart writer: %v", err)
	}

	resp := performCompressedRequest(t, engine, http.MethodPost, "/multipart", writer.FormDataContentType(), "zstd", body.Bytes())
	if resp.Code != http.StatusOK {
		t.Fatalf("expected multipart route to succeed, got status=%d body=%s", resp.Code, resp.Body.String())
	}

	var payload map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
		t.Fatalf("expected valid json payload, got %v", err)
	}
	if payload["name"] != "codex" || payload["file"] != "hello-multipart" {
		t.Fatalf("unexpected multipart payload: %+v", payload)
	}
}

func TestNormalizeEncodedRequestBodyRejectsUnsupportedEncoding(t *testing.T) {
	restoreConfig := overrideRequestBodyDecodeConfig(t, true, 1<<20, 1<<20, 64, 2)
	defer restoreConfig()

	engine := newDecodeTestEngine()
	engine.POST("/bind", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodPost, "/bind", bytes.NewReader([]byte(`{"model":"gpt-5"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	resp := httptest.NewRecorder()
	engine.ServeHTTP(resp, req)

	if resp.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("expected unsupported encoding to return 415, got status=%d body=%s", resp.Code, resp.Body.String())
	}
}

func TestNormalizeEncodedRequestBodyClosesOriginalBodyOnSuccess(t *testing.T) {
	restoreConfig := overrideRequestBodyDecodeConfig(t, true, 1<<20, 1<<20, 64, 2)
	defer restoreConfig()

	engine := newDecodeTestEngine()
	engine.POST("/bind", func(c *gin.Context) {
		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"body": string(body)})
	})

	plain := []byte(`{"model":"gpt-5-close"}`)
	compressed := mustCompressRequestBody(t, plain)
	trackedBody := &trackedReadCloser{reader: bytes.NewReader(compressed)}

	req := httptest.NewRequest(http.MethodPost, "/bind", bytes.NewReader(nil))
	req.Body = trackedBody
	req.ContentLength = int64(len(compressed))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "zstd")

	resp := httptest.NewRecorder()
	engine.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("expected successful decode to return 200, got status=%d body=%s", resp.Code, resp.Body.String())
	}
	if trackedBody.closeCalls != 1 {
		t.Fatalf("expected original request body to be closed once, got %d", trackedBody.closeCalls)
	}
}

func TestNormalizeEncodedRequestBodyClosesOriginalBodyOnFailureAndUsesDefaultEnvelope(t *testing.T) {
	restoreConfig := overrideRequestBodyDecodeConfig(t, true, 1<<20, 1<<20, 64, 2)
	defer restoreConfig()

	engine := newDecodeTestEngine()
	engine.POST("/bind", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	trackedBody := &trackedReadCloser{reader: bytes.NewReader([]byte(`{"model":"gpt-5"}`))}
	req := httptest.NewRequest(http.MethodPost, "/bind", bytes.NewReader(nil))
	req.Body = trackedBody
	req.ContentLength = int64(trackedBody.reader.Len())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")

	resp := httptest.NewRecorder()
	engine.ServeHTTP(resp, req)

	if resp.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("expected unsupported encoding to return 415, got status=%d body=%s", resp.Code, resp.Body.String())
	}
	if trackedBody.closeCalls != 1 {
		t.Fatalf("expected original request body to be closed once on failure, got %d", trackedBody.closeCalls)
	}

	var payload struct {
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
		t.Fatalf("expected valid json payload, got %v", err)
	}
	if payload.Error == nil {
		t.Fatalf("expected default responder to keep error envelope, got %s", resp.Body.String())
	}
	if payload.Error["message"] != `unsupported content encoding "gzip"` {
		t.Fatalf("unexpected error message: %+v", payload.Error)
	}
}

func TestNormalizeEncodedRequestBodyUsesMidjourneyFailureResponder(t *testing.T) {
	restoreConfig := overrideRequestBodyDecodeConfig(t, true, 1<<20, 1<<20, 64, 2)
	defer restoreConfig()

	gin.SetMode(gin.TestMode)
	logger.Logger = zap.NewNop()

	engine := gin.New()
	engine.Use(NormalizeEncodedRequestBodyWithFailureResponder(surface.MidjourneyRequestBodyDecodeFailure))
	engine.POST("/mj/submit/imagine", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodPost, "/mj/submit/imagine", bytes.NewReader([]byte(`{"model":"mj"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")

	resp := httptest.NewRecorder()
	engine.ServeHTTP(resp, req)

	if resp.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("expected unsupported encoding to preserve 415 semantics, got status=%d body=%s", resp.Code, resp.Body.String())
	}

	var payload struct {
		Description string `json:"description"`
		Type        string `json:"type"`
		Code        int    `json:"code"`
		Error       any    `json:"error"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
		t.Fatalf("expected valid json payload, got %v", err)
	}
	if payload.Error != nil {
		t.Fatalf("expected Midjourney responder to avoid generic error envelope, got %s", resp.Body.String())
	}
	if payload.Description != `unsupported content encoding "gzip"` {
		t.Fatalf("unexpected Midjourney description: %+v", payload)
	}
	if payload.Type != "one_hub_error" {
		t.Fatalf("expected Midjourney responder type one_hub_error, got %q", payload.Type)
	}
	if payload.Code != provider.MjRequestError {
		t.Fatalf("expected Midjourney responder code %d, got %d", provider.MjRequestError, payload.Code)
	}
}

func TestNormalizeEncodedRequestBodyUsesClaudeContract(t *testing.T) {
	restoreConfig := overrideRequestBodyDecodeConfig(t, true, 1<<20, 1<<20, 64, 2)
	defer restoreConfig()

	engine := gin.New()
	engine.Use(NormalizeEncodedRequestBodyWithFailureResponder(surface.ClaudeRequestBodyDecodeFailure))
	engine.POST("/claude/v1/messages", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodPost, "/claude/v1/messages", bytes.NewReader([]byte(`{"model":"claude"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")

	resp := httptest.NewRecorder()
	engine.ServeHTTP(resp, req)

	var payload struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
		t.Fatalf("expected valid json payload, got %v", err)
	}
	if payload.Error.Message != `unsupported content encoding "gzip"` {
		t.Fatalf("unexpected Claude error payload: %+v", payload)
	}
}

func TestNormalizeEncodedRequestBodyUsesGeminiContract(t *testing.T) {
	restoreConfig := overrideRequestBodyDecodeConfig(t, true, 1<<20, 1<<20, 64, 2)
	defer restoreConfig()

	engine := gin.New()
	engine.Use(NormalizeEncodedRequestBodyWithFailureResponder(surface.GeminiRequestBodyDecodeFailure))
	engine.POST("/gemini/v1/models/test:generateContent", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodPost, "/gemini/v1/models/test:generateContent", bytes.NewReader([]byte(`{"contents":[]}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")

	resp := httptest.NewRecorder()
	engine.ServeHTTP(resp, req)

	var payload struct {
		Error struct {
			Code    int    `json:"code"`
			Status  string `json:"status"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
		t.Fatalf("expected valid json payload, got %v", err)
	}
	if payload.Error.Message != `unsupported content encoding "gzip"` {
		t.Fatalf("unexpected Gemini error payload: %+v", payload)
	}
}

func TestNormalizeEncodedRequestBodyUsesRerankContract(t *testing.T) {
	restoreConfig := overrideRequestBodyDecodeConfig(t, true, 1<<20, 1<<20, 64, 2)
	defer restoreConfig()

	engine := gin.New()
	engine.Use(NormalizeEncodedRequestBodyWithFailureResponder(surface.RerankRequestBodyDecodeFailure))
	engine.POST("/v1/rerank", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/rerank", bytes.NewReader([]byte(`{"model":"rerank"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")

	resp := httptest.NewRecorder()
	engine.ServeHTTP(resp, req)

	var payload struct {
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
		t.Fatalf("expected valid json payload, got %v", err)
	}
	if payload.Detail != `unsupported content encoding "gzip"` {
		t.Fatalf("unexpected Rerank error payload: %+v", payload)
	}
}

func TestNormalizeEncodedRequestBodyUsesTaskContract(t *testing.T) {
	restoreConfig := overrideRequestBodyDecodeConfig(t, true, 1<<20, 1<<20, 64, 2)
	defer restoreConfig()

	engine := gin.New()
	engine.Use(NormalizeEncodedRequestBodyWithFailureResponder(surface.TaskRequestBodyDecodeFailure))
	engine.POST("/suno/submit/music", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodPost, "/suno/submit/music", bytes.NewReader([]byte(`{"prompt":"hello"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")

	resp := httptest.NewRecorder()
	engine.ServeHTTP(resp, req)

	var payload struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
		t.Fatalf("expected valid json payload, got %v", err)
	}
	if payload.Message != `unsupported content encoding "gzip"` {
		t.Fatalf("unexpected Task error payload: %+v", payload)
	}
	if payload.Code == "" {
		t.Fatalf("expected task error code to be populated")
	}
}

func TestNormalizeEncodedRequestBodyUsesRecraftContract(t *testing.T) {
	restoreConfig := overrideRequestBodyDecodeConfig(t, true, 1<<20, 1<<20, 64, 2)
	defer restoreConfig()

	engine := gin.New()
	engine.Use(NormalizeEncodedRequestBodyWithFailureResponder(surface.RecraftRequestBodyDecodeFailure))
	engine.POST("/recraftAI/v1/styles", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodPost, "/recraftAI/v1/styles", bytes.NewReader([]byte(`{"prompt":"hello"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")

	resp := httptest.NewRecorder()
	engine.ServeHTTP(resp, req)

	if resp.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("expected unsupported encoding to preserve 415 semantics, got status=%d body=%s", resp.Code, resp.Body.String())
	}

	var payload struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Error   any    `json:"error"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
		t.Fatalf("expected valid json payload, got %v", err)
	}
	if payload.Error != nil {
		t.Fatalf("expected Recraft responder to avoid generic error envelope, got %s", resp.Body.String())
	}
	if payload.Message != `unsupported content encoding "gzip"` {
		t.Fatalf("unexpected Recraft error payload: %+v", payload)
	}
	if payload.Code != "invalid_request" {
		t.Fatalf("expected Recraft responder code invalid_request, got %q", payload.Code)
	}
}

func TestNormalizeEncodedRequestBodyRejectsInvalidPayload(t *testing.T) {
	restoreConfig := overrideRequestBodyDecodeConfig(t, true, 1<<20, 1<<20, 64, 2)
	defer restoreConfig()

	engine := newDecodeTestEngine()
	engine.POST("/bind", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodPost, "/bind", bytes.NewReader([]byte(`not-zstd`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "zstd")
	resp := httptest.NewRecorder()
	engine.ServeHTTP(resp, req)

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("expected invalid zstd payload to return 400, got status=%d body=%s", resp.Code, resp.Body.String())
	}
}

func TestNormalizeEncodedRequestBodyRejectsDecodedLimit(t *testing.T) {
	restoreConfig := overrideRequestBodyDecodeConfig(t, true, 1<<20, 32, 64, 2)
	defer restoreConfig()

	engine := newDecodeTestEngine()
	engine.POST("/bind", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	plain := []byte(`{"model":"gpt-5","input":"this is too large for the configured limit"}`)
	resp := performCompressedRequest(t, engine, http.MethodPost, "/bind", "application/json", "zstd", plain)
	if resp.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected decoded limit to return 413, got status=%d body=%s", resp.Code, resp.Body.String())
	}
}

func TestNormalizeEncodedRequestBodyRejectsWireLimit(t *testing.T) {
	plain := []byte(`{"model":"gpt-5","input":"wire limit"}`)
	compressed := mustCompressRequestBody(t, plain)

	restoreConfig := overrideRequestBodyDecodeConfig(t, true, int64(len(compressed)-1), 1<<20, 64, 2)
	defer restoreConfig()

	engine := newDecodeTestEngine()
	engine.POST("/bind", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodPost, "/bind", bytes.NewReader(compressed))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "zstd")

	resp := httptest.NewRecorder()
	engine.ServeHTTP(resp, req)

	if resp.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected wire limit to return 413, got status=%d body=%s", resp.Code, resp.Body.String())
	}
}

func TestNormalizeEncodedRequestBodySupportsRouteLocalMounting(t *testing.T) {
	restoreConfig := overrideRequestBodyDecodeConfig(t, true, 1<<20, 1<<20, 64, 2)
	defer restoreConfig()

	gin.SetMode(gin.TestMode)
	logger.Logger = zap.NewNop()

	engine := gin.New()
	structuredRoutes := engine.Group("/v1")
	structuredRoutes.Use(NormalizeEncodedRequestBody())
	structuredRoutes.POST("/responses", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})
	engine.POST("/v1/files", func(c *gin.Context) {
		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"content_encoding": c.Request.Header.Get("Content-Encoding"),
			"body":             string(body),
		})
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/files", bytes.NewReader([]byte("opaque-wire-body")))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Encoding", "gzip")

	resp := httptest.NewRecorder()
	engine.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("expected raw relay route to bypass decode middleware, got status=%d body=%s", resp.Code, resp.Body.String())
	}

	var payload map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
		t.Fatalf("expected valid json payload, got %v", err)
	}
	if payload["content_encoding"] != "gzip" {
		t.Fatalf("expected content-encoding to remain untouched, got %#v", payload["content_encoding"])
	}
	if payload["body"] != "opaque-wire-body" {
		t.Fatalf("expected body to remain untouched, got %#v", payload["body"])
	}
}

func TestNormalizeEncodedRequestBodyDoesNotInterceptNoRoute(t *testing.T) {
	restoreConfig := overrideRequestBodyDecodeConfig(t, true, 1<<20, 1<<20, 64, 2)
	defer restoreConfig()

	gin.SetMode(gin.TestMode)
	logger.Logger = zap.NewNop()

	engine := gin.New()
	structuredRoutes := engine.Group("/v1")
	structuredRoutes.Use(NormalizeEncodedRequestBody())
	structuredRoutes.POST("/responses", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})
	engine.NoRoute(func(c *gin.Context) {
		c.Status(http.StatusNotFound)
	})

	req := httptest.NewRequest(http.MethodPost, "/missing", bytes.NewReader([]byte("opaque-wire-body")))
	req.Header.Set("Content-Encoding", "gzip")

	resp := httptest.NewRecorder()
	engine.ServeHTTP(resp, req)

	if resp.Code != http.StatusNotFound {
		t.Fatalf("expected missing route to reach NoRoute handler, got status=%d body=%s", resp.Code, resp.Body.String())
	}
}

func TestNormalizeEncodedRequestBodyCanRunAfterAuth(t *testing.T) {
	restoreConfig := overrideRequestBodyDecodeConfig(t, true, 1<<20, 1<<20, 64, 2)
	defer restoreConfig()

	gin.SetMode(gin.TestMode)
	logger.Logger = zap.NewNop()

	engine := gin.New()
	protectedRoutes := engine.Group("/secure")
	protectedRoutes.Use(func(c *gin.Context) {
		c.Status(http.StatusUnauthorized)
		c.Abort()
	}, NormalizeEncodedRequestBody())
	protectedRoutes.POST("/body", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodPost, "/secure/body", bytes.NewReader([]byte("opaque-wire-body")))
	req.Header.Set("Content-Encoding", "gzip")

	resp := httptest.NewRecorder()
	engine.ServeHTTP(resp, req)

	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("expected auth middleware to reject before decode, got status=%d body=%s", resp.Code, resp.Body.String())
	}
}

func newDecodeTestEngine() *gin.Engine {
	gin.SetMode(gin.TestMode)
	logger.Logger = zap.NewNop()

	engine := gin.New()
	engine.Use(NormalizeEncodedRequestBody())
	return engine
}

func overrideRequestBodyDecodeConfig(t *testing.T, enabled bool, maxWireBytes, maxDecodedBytes, maxExpansionRatio int64, maxLayers int) func() {
	t.Helper()

	originalEnabled := config.RequestBodyDecodeEnabled
	originalMaxWireBytes := config.RequestBodyDecodeMaxWireBytes
	originalMaxDecodedBytes := config.RequestBodyDecodeMaxDecodedBytes
	originalMaxExpansionRatio := config.RequestBodyDecodeMaxExpansionRatio
	originalMaxLayers := config.RequestBodyDecodeMaxLayers

	config.RequestBodyDecodeEnabled = enabled
	config.RequestBodyDecodeMaxWireBytes = maxWireBytes
	config.RequestBodyDecodeMaxDecodedBytes = maxDecodedBytes
	config.RequestBodyDecodeMaxExpansionRatio = maxExpansionRatio
	config.RequestBodyDecodeMaxLayers = maxLayers

	return func() {
		config.RequestBodyDecodeEnabled = originalEnabled
		config.RequestBodyDecodeMaxWireBytes = originalMaxWireBytes
		config.RequestBodyDecodeMaxDecodedBytes = originalMaxDecodedBytes
		config.RequestBodyDecodeMaxExpansionRatio = originalMaxExpansionRatio
		config.RequestBodyDecodeMaxLayers = originalMaxLayers
	}
}

func performCompressedRequest(t *testing.T, engine http.Handler, method, path, contentType, contentEncoding string, plain []byte) *httptest.ResponseRecorder {
	t.Helper()

	compressed := mustCompressRequestBody(t, plain)
	req := httptest.NewRequest(method, path, bytes.NewReader(compressed))
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Content-Encoding", contentEncoding)

	resp := httptest.NewRecorder()
	engine.ServeHTTP(resp, req)
	return resp
}

func mustCompressRequestBody(t *testing.T, plain []byte) []byte {
	t.Helper()

	var buf bytes.Buffer
	writer, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatalf("failed to create zstd writer: %v", err)
	}
	if _, err = writer.Write(plain); err != nil {
		t.Fatalf("failed to write zstd payload: %v", err)
	}
	if err = writer.Close(); err != nil {
		t.Fatalf("failed to close zstd writer: %v", err)
	}
	return buf.Bytes()
}

type trackedReadCloser struct {
	reader     *bytes.Reader
	closeCalls int
}

func (r *trackedReadCloser) Read(p []byte) (int, error) {
	return r.reader.Read(p)
}

func (r *trackedReadCloser) Close() error {
	r.closeCalls++
	return nil
}
