package relay

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestDecisionsPreservesNestedFieldsAndValidatesRoutingEnvelope(t *testing.T) {
	withNestedArrays := func(levels int) string {
		return `{"model":"gpt-6-luna","future":` + strings.Repeat("[", levels) + "null" + strings.Repeat("]", levels) + "}"
	}
	for _, tc := range []struct {
		name, body string
		valid      bool
	}{
		{"opaque nested fields", ` { "model":"gpt-6-luna", "future":{"color":1, "color":2}, "MODEL":"other" } `, true},
		{"at depth limit", withNestedArrays(63), true},
		{"over depth limit", withNestedArrays(64), false},
		{"duplicate routing model", `{"model":"gpt-6-luna","model":"other"}`, false},
		{"missing routing model", `{"MODEL":"other"}`, false},
		{"non-string routing model", `{"model":42}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(tc.body))
			r := NewRelayDecisions(ctx)
			err := r.setRequest()
			if (err == nil) != tc.valid {
				t.Fatalf("请求校验结果与预期不符：%v", err)
			}
			if tc.valid && (r.getOriginalModel() != "gpt-6-luna" || string(r.body) != tc.body) {
				t.Fatalf("请求模型或原始正文发生变化：模型 %q，正文 %s", r.getOriginalModel(), r.body)
			}
		})
	}
}
