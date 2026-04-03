package router

import (
	"testing"

	"github.com/gin-gonic/gin"
)

func TestSetApiRouterRegistersNewOptionAdminEndpoints(t *testing.T) {
	gin.SetMode(gin.TestMode)

	engine := gin.New()
	SetApiRouter(engine)

	want := map[string]bool{
		"GET /api/option/channel_affinity_cache":    false,
		"GET /api/option/execution_session_cache":   false,
		"DELETE /api/option/channel_affinity_cache": false,
	}

	for _, route := range engine.Routes() {
		key := route.Method + " " + route.Path
		if _, ok := want[key]; ok {
			want[key] = true
		}
	}

	for route, found := range want {
		if !found {
			t.Fatalf("expected route %s to be registered", route)
		}
	}
}
