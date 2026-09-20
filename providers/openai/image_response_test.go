package openai

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"one-api/common"
	"one-api/common/config"
	"one-api/common/requester"
	"one-api/model"
	"one-api/types"
)

func TestImageEditsJSONModelPatchPreservesUnionsAndDoesNotApplyCustomParams(t *testing.T) {
	raw := []byte(` {"model":"client","prompt":"draw","images":[{"image_url":"https://image.test/a"},"future"],"mask":{"future_union":true},"future":{"n":1e+02}} `)
	for _, mapped := range []string{"client", "mapped"} {
		t.Run(mapped, func(t *testing.T) {
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/images/edits", bytes.NewReader(raw))
			ctx.Request.Header.Set("Content-Type", "application/json")
			if _, err := common.CacheRequestBody(ctx); err != nil {
				t.Fatal(err)
			}
			proxy := ""
			custom := `{"prompt":"must not replace","stream":true}`
			p := CreateOpenAIProvider(&model.Channel{Type: config.ChannelTypeOpenAI, Key: "test", Proxy: &proxy, CustomParameter: &custom}, "https://api.openai.com")
			p.SetContext(ctx)
			p.SetOriginalModel("client")
			var request types.ImageEditRequest
			if err := json.Unmarshal(raw, &request); err != nil {
				t.Fatalf("image union projection: %v", err)
			}
			request.Model = mapped
			req, apiErr := p.getRequestImageBody(config.RelayModeImagesEdits, mapped, &request)
			if apiErr != nil {
				t.Fatal(apiErr)
			}
			defer req.Body.Close()
			got, _ := io.ReadAll(req.Body)
			if mapped == "client" && !bytes.Equal(raw, got) {
				t.Fatalf("unmapped bytes changed: %q", got)
			}
			if !bytes.Contains(got, []byte(`"future":{"n":1e+02}`)) || !bytes.Contains(got, []byte(`"prompt":"draw"`)) || bytes.Contains(got, []byte(`"stream"`)) {
				t.Fatalf("unrelated JSON patched: %s", got)
			}
			var fields map[string]json.RawMessage
			_ = json.Unmarshal(got, &fields)
			if string(fields["model"]) != `"`+mapped+`"` {
				t.Fatalf("model not mapped: %s", got)
			}
			if req.Header.Get("Content-Type") != "application/json" {
				t.Fatalf("content type=%q", req.Header.Get("Content-Type"))
			}
		})
	}
}

func TestNativeImageResponseDispatchesActualMediaTypeOnce(t *testing.T) {
	for _, sse := range []bool{false, true} {
		t.Run(map[bool]string{false: "JSON despite stream", true: "SSE despite unary intent"}[sse], func(t *testing.T) {
			calls := 0
			wire := `{"data":[{"future_image":true}],"future":{"kept":true}}`
			if sse {
				wire = "event: future\ndata: {\"future\":true}\n\n"
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if sse {
					w.Header().Set("Content-Type", "text/event-stream")
				} else {
					w.Header().Set("Content-Type", "application/json")
				}
				io.WriteString(w, wire)
			}))
			defer server.Close()
			old := requester.HTTPClient
			requester.HTTPClient = server.Client()
			defer func() { requester.HTTPClient = old }()
			raw := `{"model":"gpt-image-1","prompt":"draw","stream":true}`
			if sse {
				raw = strings.Replace(raw, "true", "false", 1)
			}
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(raw))
			ctx.Request.Header.Set("Content-Type", "application/json")
			if _, err := common.CacheRequestBody(ctx); err != nil {
				t.Fatal(err)
			}
			proxy := ""
			baseURL := server.URL
			p := CreateOpenAIProvider(&model.Channel{Type: config.ChannelTypeOpenAI, Key: "test", Proxy: &proxy, BaseURL: &baseURL}, server.URL)
			p.SetContext(ctx)
			p.SetUsage(&types.Usage{})
			result, apiErr := p.CreateImageGenerationsResponse(&types.ImageRequest{Model: "gpt-image-1", Prompt: "draw"})
			if apiErr != nil || calls != 1 || result == nil {
				t.Fatalf("result=%+v err=%+v calls=%d", result, apiErr, calls)
			}
			if sse {
				if result.Stream == nil {
					t.Fatal("SSE misclassified")
				}
				defer result.Stream.Body.Close()
				got, _ := io.ReadAll(result.Stream.Body)
				if string(got) != wire {
					t.Fatalf("SSE bytes=%q", got)
				}
			} else {
				if result.JSON == nil || string(result.JSON.ReplayProviderRawJSON()) != wire {
					t.Fatalf("JSON replay=%+v", result)
				}
				if p.Usage.ProviderOperationUnits == nil || *p.Usage.ProviderOperationUnits != 1 {
					t.Fatalf("confirmed JSON image count: %+v", p.Usage)
				}
			}
		})
	}
}

func TestImageStreamEvidenceOnlyCountsFinalSnapshotsOnce(t *testing.T) {
	usage := &types.Usage{}
	observer := &imageStreamEvidence{target: usage}
	observer.Observe([]byte(`{"type":"image_generation.partial_image","b64_json":"preview","usage":{"input_tokens":1000}}`))
	if usage.ProviderOperationUnits != nil || usage.HasProviderUsage() {
		t.Fatalf("preview was billed: %+v", usage)
	}
	done := []byte(`{"type":"image_generation.completed","model":"gpt-image-1","b64_json":"image","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15,"input_tokens_details":{"text_tokens":6,"image_tokens":4}}}`)
	observer.Observe(done)
	observer.Observe(done)
	if usage.ProviderOperationUnits == nil || *usage.ProviderOperationUnits != 1 || usage.TotalTokens != 15 || !usage.HasProviderUsage() {
		t.Fatalf("final evidence=%+v", usage)
	}
	observer.Observe(bytes.Replace(done, []byte(`"output_tokens":5,"total_tokens":15`), []byte(`"output_tokens":6,"total_tokens":16`), 1))
	if !usage.ProviderTokenConflict || usage.ProviderOperationUnits == nil || *usage.ProviderOperationUnits != 1 {
		t.Fatalf("conflicting tokens damaged independent operation or remained billable: %+v", usage)
	}
	observer.Observe(done)
	if !usage.ProviderTokenConflict {
		t.Fatal("late duplicate cleared evidence conflict")
	}
}

func TestNativeImageJSONBusinessErrorPreservesBodyAndStatus(t *testing.T) {
	wire := `{ "error":{"message":"upstream rejected request","code":"future_code"}, "data":[{"b64_json":"not_billable"}], "future":1e3 }`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, wire)
	}))
	defer server.Close()
	old := requester.HTTPClient
	requester.HTTPClient = server.Client()
	defer func() { requester.HTTPClient = old }()
	proxy := ""
	baseURL := server.URL
	p := CreateOpenAIProvider(&model.Channel{Type: config.ChannelTypeOpenAI, Key: "test", Proxy: &proxy, BaseURL: &baseURL}, server.URL)
	p.SetUsage(&types.Usage{})
	result, apiErr := p.CreateImageGenerationsResponse(&types.ImageRequest{Model: "image", Prompt: "draw"})
	if apiErr != nil || result == nil || result.JSON == nil || string(result.JSON.ReplayProviderRawJSON()) != wire {
		t.Fatalf("provider error wire changed: result=%+v err=%+v", result, apiErr)
	}
	if p.Usage.HasProviderUsage() || p.Usage.ProviderOperationUnits != nil {
		t.Fatalf("error body became evidence: %+v", p.Usage)
	}
}
