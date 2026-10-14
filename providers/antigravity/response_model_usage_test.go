package antigravity

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"one-api/providers/antigravity/wire"
)

func TestProtocolRetainsResponseModelEvidence(t *testing.T) {
	const actualModel = "actual-priced-model"
	const usage = `"usageMetadata":` + protocolUsageJSON
	const terminal = `"candidates":[{"index":0,"finishReason":"STOP"}]`
	for _, test := range []struct {
		name      string
		responses []string
		model     string
		conflict  bool
		failure   bool
	}{
		{name: "missing model stays unknown", responses: []string{`{` + terminal + `,` + usage + `}`}},
		{name: "reported model", responses: []string{`{` + terminal + `,` + usage + `,"modelVersion":"actual-priced-model"}`}, model: actualModel},
		{name: "unknown model shape", responses: []string{`{` + terminal + `,` + usage + `,"modelVersion":{"future":true}}`}},
		{name: "model before usage", responses: []string{`{"modelVersion":"actual-priced-model"}`, `{` + terminal + `,` + usage + `}`}, model: actualModel},
		{name: "model after usage", responses: []string{`{` + terminal + `,` + usage + `}`, `{"modelVersion":"actual-priced-model"}`}, model: actualModel},
		{name: "unknown model shape preserves earlier evidence", responses: []string{`{"modelVersion":"actual-priced-model"}`, `{` + terminal + `,` + usage + `,"modelVersion":{"future":true}}`}, model: actualModel},
		{name: "conflict survives trailing usage", responses: []string{`{"modelVersion":"actual-priced-model"}`, `{"modelVersion":"other-model"}`, `{` + terminal + `,` + usage + `}`}, model: actualModel, conflict: true},
		{name: "error retains model and usage", responses: []string{`{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"executed"},` + usage + `,"modelVersion":"actual-priced-model"}`}, model: actualModel, failure: true},
		{name: "error has no response model", responses: []string{`{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"executed"},` + usage + `}`}, failure: true},
	} {
		for _, dialect := range []wire.Dialect{wire.DialectGemini, wire.DialectClaude, wire.DialectChat, wire.DialectResponses} {
			for _, streaming := range []bool{false, true} {
				if dialect == wire.DialectGemini && !streaming && len(test.responses) > 1 {
					continue
				}
				t.Run(fmt.Sprintf("%s/%s/stream=%t", test.name, dialect, streaming), func(t *testing.T) {
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						if dialect == wire.DialectGemini && !streaming {
							w.Header().Set("Content-Type", "application/json")
							_, _ = io.WriteString(w, `{"modelVersion":"envelope-model","response":`+test.responses[0]+`}`)
							return
						}
						w.Header().Set("Content-Type", "text/event-stream")
						for _, response := range test.responses {
							_, _ = io.WriteString(w, "data: {\"modelVersion\":\"envelope-model\",\"response\":"+response+"}\n\n")
						}
					}))
					defer server.Close()
					input := protocolInput(dialect)
					provider := protocolProvider(t, server.URL, input)
					output, apiErr, streamErr := callProtocol(t, provider, dialect, streaming, input)
					if failed := apiErr != nil || streamErr != nil; failed != test.failure {
						t.Fatalf("响应模型观察改变交付结果：apiErr=%v streamErr=%v", apiErr, streamErr)
					}
					got := provider.Usage
					if got == nil || got.HasProviderUsage() == test.conflict || got.ResponseModel != test.model || got.AttributionConflict != test.conflict || got.BillingDiagnostics["billing_model_conflict"] != test.conflict {
						t.Fatalf("模型证据错误：usage=%+v，期望 model=%q conflict=%t", got, test.model, test.conflict)
					}
					if dialect == wire.DialectGemini && !test.failure && !bytes.Contains(output, []byte(test.responses[0])) {
						t.Fatalf("模型观察改写原生响应：%s", output)
					}
				})
			}
		}
	}
}
