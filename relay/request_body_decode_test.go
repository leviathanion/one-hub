package relay

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"one-api/common/config"
	"one-api/common/logger"
	"one-api/middleware"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"go.uber.org/zap"
)

func TestRelayResponsesSetRequestSupportsEncodedBody(t *testing.T) {
	originalEnabled := config.RequestBodyDecodeEnabled
	originalMaxWireBytes := config.RequestBodyDecodeMaxWireBytes
	originalMaxDecodedBytes := config.RequestBodyDecodeMaxDecodedBytes
	originalMaxExpansionRatio := config.RequestBodyDecodeMaxExpansionRatio
	originalMaxLayers := config.RequestBodyDecodeMaxLayers
	t.Cleanup(func() {
		config.RequestBodyDecodeEnabled = originalEnabled
		config.RequestBodyDecodeMaxWireBytes = originalMaxWireBytes
		config.RequestBodyDecodeMaxDecodedBytes = originalMaxDecodedBytes
		config.RequestBodyDecodeMaxExpansionRatio = originalMaxExpansionRatio
		config.RequestBodyDecodeMaxLayers = originalMaxLayers
	})

	config.RequestBodyDecodeEnabled = true
	config.RequestBodyDecodeMaxWireBytes = 1 << 20
	config.RequestBodyDecodeMaxDecodedBytes = 1 << 20
	config.RequestBodyDecodeMaxExpansionRatio = 64
	config.RequestBodyDecodeMaxLayers = 2
	logger.Logger = zap.NewNop()

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	structuredRoutes := engine.Group("/v1")
	structuredRoutes.Use(middleware.NormalizeEncodedRequestBody())
	structuredRoutes.POST("/responses", func(c *gin.Context) {
		relay := NewRelayResponses(c)
		if err := relay.setRequest(); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"model": relay.responsesRequest.Model,
			"input": relay.responsesRequest.Input,
		})
	})

	plain := []byte(`{"model":"gpt-5","input":"hello relay"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(mustCompressRelayBody(t, plain)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "zstd")

	resp := httptest.NewRecorder()
	engine.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("expected relay responses setRequest to succeed, got status=%d body=%s", resp.Code, resp.Body.String())
	}

	var payload map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
		t.Fatalf("expected valid json response, got %v", err)
	}
	if payload["model"] != "gpt-5" || payload["input"] != "hello relay" {
		t.Fatalf("unexpected relay payload: %+v", payload)
	}
}

func mustCompressRelayBody(t *testing.T, plain []byte) []byte {
	t.Helper()

	var buf bytes.Buffer
	writer, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatalf("failed to create zstd writer: %v", err)
	}
	if _, err = writer.Write(plain); err != nil {
		t.Fatalf("failed to write relay zstd payload: %v", err)
	}
	if err = writer.Close(); err != nil {
		t.Fatalf("failed to close relay zstd writer: %v", err)
	}
	return buf.Bytes()
}
