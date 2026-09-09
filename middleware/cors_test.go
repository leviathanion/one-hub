package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/spf13/viper"
)

func TestCORSDefaultCompatibilityAllowsCredentialsAndWildcardHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	originalAllowOrigins := viper.Get("cors.allow_origins")
	originalAllowCredentials := viper.Get("cors.allow_credentials")
	originalAllowHeaders := viper.Get("cors.allow_headers")
	t.Cleanup(func() {
		viper.Set("cors.allow_origins", originalAllowOrigins)
		viper.Set("cors.allow_credentials", originalAllowCredentials)
		viper.Set("cors.allow_headers", originalAllowHeaders)
	})

	viper.Set("cors.allow_origins", []string{})
	viper.Set("cors.allow_credentials", true)
	viper.Set("cors.allow_headers", []string{})

	router := gin.New()
	router.Use(CORS())
	router.GET("/ping", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodOptions, "/ping", nil)
	req.Header.Set("Origin", "https://legacy.example")
	req.Header.Set("Access-Control-Request-Method", http.MethodGet)
	req.Header.Set("Access-Control-Request-Headers", "x-any-legacy-header")
	recorder := httptest.NewRecorder()

	router.ServeHTTP(recorder, req)

	if got := recorder.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("expected legacy wildcard allow origin, got %q", got)
	}
	if got := recorder.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Fatalf("expected legacy credentials compatibility, got %q", got)
	}
	if got := recorder.Header().Get("Access-Control-Allow-Headers"); got != "*" {
		t.Fatalf("expected legacy wildcard allow headers, got %q", got)
	}
}

func TestCORSWildcardOriginDisablesCredentials(t *testing.T) {
	gin.SetMode(gin.TestMode)
	originalAllowOrigins := viper.Get("cors.allow_origins")
	originalAllowCredentials := viper.Get("cors.allow_credentials")
	t.Cleanup(func() {
		viper.Set("cors.allow_origins", originalAllowOrigins)
		viper.Set("cors.allow_credentials", originalAllowCredentials)
	})

	viper.Set("cors.allow_origins", []string{"*"})
	viper.Set("cors.allow_credentials", true)

	router := gin.New()
	router.Use(CORS())
	router.GET("/ping", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodOptions, "/ping", nil)
	req.Header.Set("Origin", "https://app.example")
	req.Header.Set("Access-Control-Request-Method", http.MethodGet)
	req.Header.Set("Access-Control-Request-Headers", "authorization,sec-websocket-protocol")
	recorder := httptest.NewRecorder()

	router.ServeHTTP(recorder, req)

	if got := recorder.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("expected wildcard allow origin, got %q", got)
	}
	if got := recorder.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Fatalf("expected wildcard origin to suppress credentials, got %q", got)
	}
}

func TestCORSIncludesProviderSpecificHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	originalAllowOrigins := viper.Get("cors.allow_origins")
	t.Cleanup(func() {
		viper.Set("cors.allow_origins", originalAllowOrigins)
	})

	viper.Set("cors.allow_origins", []string{"https://app.example"})

	router := gin.New()
	router.Use(CORS())
	router.GET("/ping", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodOptions, "/ping", nil)
	req.Header.Set("Origin", "https://app.example")
	req.Header.Set("Access-Control-Request-Method", http.MethodGet)
	req.Header.Set("Access-Control-Request-Headers", "anthropic-version,x-goog-api-key,authorization,openai-organization,openai-project,openai-safety-identifier")
	recorder := httptest.NewRecorder()

	router.ServeHTTP(recorder, req)

	allowHeaders := strings.ToLower(recorder.Header().Get("Access-Control-Allow-Headers"))
	for _, header := range []string{"anthropic-version", "x-goog-api-key", "authorization", "openai-organization", "openai-project", "openai-safety-identifier", "sec-websocket-protocol"} {
		if !strings.Contains(allowHeaders, strings.ToLower(header)) {
			t.Fatalf("expected Access-Control-Allow-Headers to include %q, got %q", header, allowHeaders)
		}
	}
}

func TestCORSExplicitOriginPreflightAllowsIdempotencyKeyAndVariesOnRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	originalAllowOrigins := viper.Get("cors.allow_origins")
	originalAllowHeaders := viper.Get("cors.allow_headers")
	t.Cleanup(func() {
		viper.Set("cors.allow_origins", originalAllowOrigins)
		viper.Set("cors.allow_headers", originalAllowHeaders)
	})
	viper.Set("cors.allow_origins", []string{"https://app.example"})
	viper.Set("cors.allow_headers", []string{})

	router := gin.New()
	router.Use(CORS())
	router.POST("/v1/responses", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	req := httptest.NewRequest(http.MethodOptions, "/v1/responses", nil)
	req.Header.Set("Origin", "https://app.example")
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	req.Header.Set("Access-Control-Request-Headers", "authorization,idempotency-key")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("preflight status=%d, want %d", recorder.Code, http.StatusNoContent)
	}
	if got := recorder.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example" {
		t.Fatalf("allow origin=%q", got)
	}
	if got := recorder.Header().Get("Access-Control-Allow-Headers"); !corsHeaderContainsToken([]string{got}, "idempotency-key") {
		t.Fatalf("Idempotency-Key missing from Access-Control-Allow-Headers: %q", got)
	}
	for _, name := range []string{"origin", "access-control-request-method", "access-control-request-headers"} {
		if !corsHeaderContainsToken(recorder.Header().Values("Vary"), name) {
			t.Fatalf("Vary does not include %q: %v", name, recorder.Header().Values("Vary"))
		}
	}
}

func corsHeaderContainsToken(values []string, want string) bool {
	for _, value := range values {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), want) {
				return true
			}
		}
	}
	return false
}

func TestCORSAcceptsConfiguredAllowHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	originalAllowOrigins := viper.Get("cors.allow_origins")
	originalAllowHeaders := viper.Get("cors.allow_headers")
	t.Cleanup(func() {
		viper.Set("cors.allow_origins", originalAllowOrigins)
		viper.Set("cors.allow_headers", originalAllowHeaders)
	})

	viper.Set("cors.allow_origins", []string{"https://app.example"})
	viper.Set("cors.allow_headers", []string{"X-Custom-Gateway-Header", "idempotency-key"})

	router := gin.New()
	router.Use(CORS())
	router.GET("/ping", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodOptions, "/ping", nil)
	req.Header.Set("Origin", "https://app.example")
	req.Header.Set("Access-Control-Request-Method", http.MethodGet)
	req.Header.Set("Access-Control-Request-Headers", "x-custom-gateway-header")
	recorder := httptest.NewRecorder()

	router.ServeHTTP(recorder, req)

	allowHeaders := strings.ToLower(recorder.Header().Get("Access-Control-Allow-Headers"))
	if !strings.Contains(allowHeaders, "x-custom-gateway-header") {
		t.Fatalf("expected Access-Control-Allow-Headers to include configured header, got %q", allowHeaders)
	}
	configured := ConfiguredCORSAllowHeaders()
	idempotencyCount := 0
	for _, header := range configured {
		if strings.EqualFold(header, "Idempotency-Key") {
			idempotencyCount++
		}
	}
	if idempotencyCount != 1 {
		t.Fatalf("configured headers contain Idempotency-Key %d times: %v", idempotencyCount, configured)
	}
}
