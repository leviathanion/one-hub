package model

import (
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestModelsDevOwnershipUsesServingProviderForHostedModels(t *testing.T) {
	upstream, err := decodeModelsDevSource(strings.NewReader(`{
		"cloudflare-workers-ai":{"name":"Cloudflare Workers AI","models":{
			"@cf/moonshotai/kimi-k2.6":{"canonical_model_id":"moonshotai/kimi-k2.6"},
			"@cf/moonshotai/kimi-k2.7-code":{"canonical_model_id":"moonshotai/kimi-k2.7-code"},
			"@cf/nvidia/nemotron-3-120b-a12b":{"canonical_model_id":"nvidia/nemotron-3-super-120b-a12b"}
		}},
		"moonshotai":{"name":"Moonshot AI","models":{
			"kimi-k2.6":{"canonical_model_id":"moonshotai/kimi-k2.6"}
		}}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	models := []string{"@cf/moonshotai/kimi-k2.6", "@cf/moonshotai/kimi-k2.7-code", "@cf/nvidia/nemotron-3-120b-a12b", "kimi-k2.6"}
	got := modelsDevOwnershipSuggestions(upstream, models)
	cloudflare := []ModelOwnershipProvider{{ID: "cloudflare-workers-ai", Name: "Cloudflare Workers AI"}}
	want := []ModelOwnershipSuggestion{
		{Model: models[0], Providers: cloudflare, Status: "matched"},
		{Model: models[1], Providers: cloudflare, Status: "matched"},
		{Model: models[2], Providers: cloudflare, Status: "matched"},
		{Model: models[3], Providers: []ModelOwnershipProvider{{ID: "moonshotai", Name: "Moonshot AI"}}, Status: "matched"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("suggestions=%+v, want=%+v", got, want)
	}
}

func TestModelsDevOwnershipUsesExactProviderEvidenceWithoutCanonicalOrPrice(t *testing.T) {
	upstream, err := decodeModelsDevSource(strings.NewReader(`{
		"host-a":{"name":"Same display name","models":{
			"no-price":{"canonical_model_id":"anthropic/canonical"},
			"bad-price":{"canonical_model_id":"google/canonical","cost":{"input":1,"future_price":5}},
			"same-author":{"canonical_model_id":"openai/a"},
			"different-authors":{"canonical_model_id":"openai/a"},
			"Case":{"canonical_model_id":"meta/model"},
			"host/model":{"canonical_model_id":"deepseek/model"},
			"malformed":{"canonical_model_id":"missing-slash"},
			"no-canonical":{}
		}},
		"host-b":{"name":"Same display name","models":{
			"same-author":{"canonical_model_id":"openai/a"},
			"different-authors":{"canonical_model_id":"anthropic/a"}
		}},
		"unnamed":{"models":{"unnamed-model":{}}},
		"blank-name":{"name":" \t ","models":{"blank-name-model":{}}}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	models := []string{"no-price", "bad-price", "same-author", "different-authors", "Case", "case", "host/model", "model", "malformed", "no-canonical", "missing", "unnamed-model", "blank-name-model"}
	got := modelsDevOwnershipSuggestions(upstream, models)
	hostA := ModelOwnershipProvider{ID: "host-a", Name: "Same display name"}
	hostB := ModelOwnershipProvider{ID: "host-b", Name: "Same display name"}
	want := []ModelOwnershipSuggestion{
		{Model: "no-price", Providers: []ModelOwnershipProvider{hostA}, Status: "matched"},
		{Model: "bad-price", Providers: []ModelOwnershipProvider{hostA}, Status: "matched"},
		{Model: "same-author", Providers: []ModelOwnershipProvider{hostA, hostB}, Status: "conflict"},
		{Model: "different-authors", Providers: []ModelOwnershipProvider{hostA, hostB}, Status: "conflict"},
		{Model: "Case", Providers: []ModelOwnershipProvider{hostA}, Status: "matched"},
		{Model: "case", Providers: []ModelOwnershipProvider{}, Status: "unmatched"},
		{Model: "host/model", Providers: []ModelOwnershipProvider{hostA}, Status: "matched"},
		{Model: "model", Providers: []ModelOwnershipProvider{}, Status: "unmatched"},
		{Model: "malformed", Providers: []ModelOwnershipProvider{hostA}, Status: "matched"},
		{Model: "no-canonical", Providers: []ModelOwnershipProvider{hostA}, Status: "matched"},
		{Model: "missing", Providers: []ModelOwnershipProvider{}, Status: "unmatched"},
		{Model: "unnamed-model", Providers: []ModelOwnershipProvider{{ID: "unnamed", Name: "unnamed"}}, Status: "matched"},
		{Model: "blank-name-model", Providers: []ModelOwnershipProvider{{ID: "blank-name", Name: "blank-name"}}, Status: "matched"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("suggestions=%+v, want=%+v", got, want)
	}
	prices := convertModelsDevPrices(upstream)
	for _, price := range prices.Prices {
		if price.Model == "no-price" || price.Model == "bad-price" {
			t.Fatalf("ownership evidence admitted an invalid price: %+v", price)
		}
	}
}

func TestModelsDevOwnershipFetchValidatesBeforeNetworkAndDeduplicates(t *testing.T) {
	calls := 0
	client := newModelsDevClient()
	client.Transport = modelsDevRoundTrip(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.URL.String() != ModelsDevURL || req.Method != http.MethodGet {
			t.Fatalf("unexpected request: %+v", req)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"host":{"models":{"a":{"canonical_model_id":"lab/model"}}}}`))}, nil
	})
	for _, names := range [][]string{nil, {""}, {"family*"}, {strings.Repeat("a", 101)}, make([]string, ModelOwnershipBatchLimit+1)} {
		_, err := fetchModelsDevOwnershipSuggestions(context.Background(), client, names)
		var validationErr *ModelOwnershipValidationError
		if !errors.As(err, &validationErr) {
			t.Fatalf("models=%v error=%v, expected validation error", names, err)
		}
	}
	if calls != 0 {
		t.Fatalf("invalid input contacted source %d times", calls)
	}
	got, err := fetchModelsDevOwnershipSuggestions(context.Background(), client, []string{"z", "a", "a"})
	if err != nil {
		t.Fatal(err)
	}
	want := []ModelOwnershipSuggestion{{Model: "a", Providers: []ModelOwnershipProvider{{ID: "host", Name: "host"}}, Status: "matched"}, {Model: "z", Providers: []ModelOwnershipProvider{}, Status: "unmatched"}}
	if calls != 1 || !reflect.DeepEqual(got, want) {
		t.Fatalf("calls=%d suggestions=%+v", calls, got)
	}
}

func TestModelsDevOwnershipFetchPreservesSourceFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{name: "unavailable", status: http.StatusServiceUnavailable, body: `{}`},
		{name: "malformed JSON", status: http.StatusOK, body: `{`},
		{name: "empty catalog", status: http.StatusOK, body: `{}`},
		{name: "oversized catalog", status: http.StatusOK, body: strings.Repeat(" ", int(MaxRemotePriceCatalogBytes)+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newModelsDevClient()
			client.Transport = modelsDevRoundTrip(func(req *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})
			if got, err := fetchModelsDevOwnershipSuggestions(context.Background(), client, []string{"model"}); err == nil || got != nil {
				t.Fatalf("source failure became unmatched success: got=%+v error=%v", got, err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := newModelsDevClient()
	client.Transport = modelsDevRoundTrip(func(req *http.Request) (*http.Response, error) { return nil, req.Context().Err() })
	if _, err := fetchModelsDevOwnershipSuggestions(ctx, client, []string{"model"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}

func TestModelsDevOwnershipFetchDoesNotFollowRedirect(t *testing.T) {
	calls := 0
	client := newModelsDevClient()
	client.Transport = modelsDevRoundTrip(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"https://other.example/catalog"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	if _, err := fetchModelsDevOwnershipSuggestions(context.Background(), client, []string{"model"}); err == nil || calls != 1 {
		t.Fatalf("redirect was accepted: calls=%d error=%v", calls, err)
	}
}
