package controller

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"one-api/model"
	"strings"
	"testing"
)

type modelsDevTransport func(*http.Request) (*http.Response, error)

func (f modelsDevTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestModelsDevFetchPreviewApplyHTTPChain(t *testing.T) {
	router := setupPricingControllerTest(t)
	router.GET("/modelsdev", GetModelsDevPrices)
	old := http.DefaultTransport
	http.DefaultTransport = modelsDevTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != model.ModelsDevURL {
			t.Fatal(req.URL)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"openai":{"models":{"free":{"cost":{"input":0,"output":4}},"missing":{"cost":{"input":2}}}}}`)), Header: make(http.Header)}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = old })
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest("GET", "/modelsdev", nil))
	var fetched struct {
		Success bool                   `json:"success"`
		Data    model.ModelsDevCatalog `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &fetched); err != nil || !fetched.Success {
		t.Fatal(recorder.Body.String(), err)
	}
	if len(fetched.Data.Prices) != 1 || fetched.Data.Skipped != 1 || len(fetched.Data.Candidates) != 2 {
		t.Fatalf("unexpected sync catalog: %+v", fetched.Data)
	}
	source := fetched.Data.Prices
	previewBody, _ := json.Marshal(map[string]any{"mode": "add", "source": source})
	response := performPricingRequest(t, router, "/preview", string(previewBody))
	var preview struct {
		Success bool                     `json:"success"`
		Data    model.PriceChangePreview `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &preview); err != nil || !preview.Success {
		t.Fatal(response.Body.String(), err)
	}
	applyBody, _ := json.Marshal(map[string]any{"mode": "add", "source": source, "base_version": preview.Data.BaseVersion, "digest": preview.Data.Digest})
	response = performPricingRequest(t, router, "/apply", string(applyBody))
	var applied struct {
		Success bool  `json:"success"`
		Version int64 `json:"version"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &applied); err != nil || !applied.Success || applied.Version != 2 {
		t.Fatal(response.Body.String(), err)
	}
	p, ok := model.PricingInstance.FindExactPrice("free")
	if !ok || p.Input != 0 || p.Output != 2 {
		t.Fatal(p, ok)
	}
	response = performPricingRequest(t, router, "/apply", string(applyBody))
	if response.Code != http.StatusConflict {
		t.Fatal(response.Code, response.Body.String())
	}
}
