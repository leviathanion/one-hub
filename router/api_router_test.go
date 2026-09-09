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
		"PUT /api/option/batch":                     false,
		"POST /api/user/dashboard/modules/query":    false,
		"GET /api/readyz":                           false,
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

func TestOpenAIRouterRegistersStableUnsupportedCapabilityRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	setOpenAIRouter(engine)

	want := map[string]bool{
		"POST /v1/responses/:response_id/cancel": false,
		"POST /v1/responses/:response_id/resume": false,
		"POST /v1/uploads":                       false,
		"POST /v1/uploads/*any":                  false,
		"POST /v1/conversations":                 false,
		"POST /v1/conversations/*any":            false,
		"POST /v1/vector_stores":                 false,
		"POST /v1/vector_stores/*any":            false,
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

func TestLegacyMidjourneyImageProxyRoutesAreAbsent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	SetApiRouter(engine)
	setMJRouter(engine)

	routes := make(map[string]struct{})
	for _, route := range engine.Routes() {
		routes[route.Method+" "+route.Path] = struct{}{}
	}
	for _, removed := range []string{
		"GET /mj/image/:id",
		"GET /:mode/mj/image/:id",
		"POST /mj/notify",
		"POST /:mode/mj/notify",
	} {
		if _, exists := routes[removed]; exists {
			t.Fatalf("legacy media proxy route remains registered: %s", removed)
		}
	}
	for _, retained := range []string{
		"GET /mj/task/:id/fetch",
		"GET /:mode/mj/task/:id/fetch",
		"GET /mj/task/:id/image-seed",
		"GET /:mode/mj/task/:id/image-seed",
		"POST /mj/submit/imagine",
		"POST /:mode/mj/submit/imagine",
		"POST /mj/task/list-by-condition",
		"POST /:mode/mj/task/list-by-condition",
		"GET /api/image/:id",
	} {
		if _, exists := routes[retained]; !exists {
			t.Fatalf("legitimate image/task route was removed: %s", retained)
		}
	}
}
