package middleware

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestRecoveryPropagatesHTTPAbortHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(Recovery())
	router.Use(RelayPanicRecover())
	router.GET("/", func(c *gin.Context) {
		c.Status(http.StatusOK)
		_, _ = c.Writer.Write([]byte("partial"))
		panic(http.ErrAbortHandler)
	})
	server := httptest.NewServer(router)
	defer server.Close()

	response, err := server.Client().Get(server.URL)
	if err != nil {
		return
	}
	defer response.Body.Close()
	if _, err := io.ReadAll(response.Body); err == nil {
		t.Fatal("partially committed response must end with a transport error")
	}
}

func TestRecoveryKeepsOrdinaryPanicAs500(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(Recovery())
	router.GET("/", func(*gin.Context) { panic("boom") })
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("ordinary panic status=%d, want 500", recorder.Code)
	}
}
