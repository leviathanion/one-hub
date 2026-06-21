package controller

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
)

func TestCurrentSessionUserID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name string
		id   any
		want int
		ok   bool
	}{
		{name: "valid int", id: 42, want: 42, ok: true},
		{name: "missing"},
		{name: "wrong type", id: "42"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			router := gin.New()
			router.Use(sessions.Sessions("session", cookie.NewStore([]byte("session-helper-test-secret"))))
			router.GET("/session", func(c *gin.Context) {
				if tt.id != nil {
					sessions.Default(c).Set("id", tt.id)
				}

				got, ok := currentSessionUserID(c)
				if ok != tt.ok || got != tt.want {
					t.Fatalf("expected id=%d ok=%v, got id=%d ok=%v", tt.want, tt.ok, got, ok)
				}
				c.Status(http.StatusNoContent)
			})

			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/session", nil)
			router.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusNoContent {
				t.Fatalf("expected helper test handler to complete, got %d body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}
