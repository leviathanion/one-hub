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

func TestModelsDevOwnershipUsesExactCanonicalEvidenceWithoutPrice(t *testing.T) {
	upstream, err := decodeModelsDevSource(strings.NewReader(`{
		"host-a":{"models":{
			"no-price":{"canonical_model_id":"anthropic/canonical"},
			"bad-price":{"canonical_model_id":"google/canonical","cost":{"input":1,"future_price":5}},
			"same":{"canonical_model_id":"openai/a"},
			"conflict":{"canonical_model_id":"openai/a"},
			"Case":{"canonical_model_id":"meta/model"},
			"host/model":{"canonical_model_id":"deepseek/model"},
			"malformed":{"canonical_model_id":"missing-slash"}
		}},
		"host-b":{"models":{
			"same":{"canonical_model_id":"openai/a"},
			"conflict":{"canonical_model_id":"anthropic/a"},
			"no-price":{}
		}},
		"openai":{"models":{"no-canonical":{"cost":{"input":1,"output":2}}}}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	models := []string{"no-price", "bad-price", "same", "conflict", "Case", "case", "host/model", "model", "malformed", "no-canonical", "missing"}
	got := modelsDevOwnershipSuggestions(upstream, models)
	want := []ModelOwnershipSuggestion{
		{Model: "no-price", Labs: []string{"anthropic"}, Status: "matched"},
		{Model: "bad-price", Labs: []string{"google"}, Status: "matched"},
		{Model: "same", Labs: []string{"openai"}, Status: "matched"},
		{Model: "conflict", Labs: []string{"anthropic", "openai"}, Status: "conflict"},
		{Model: "Case", Labs: []string{"meta"}, Status: "matched"},
		{Model: "case", Labs: []string{}, Status: "unmatched"},
		{Model: "host/model", Labs: []string{"deepseek"}, Status: "matched"},
		{Model: "model", Labs: []string{}, Status: "unmatched"},
		{Model: "malformed", Labs: []string{}, Status: "unmatched"},
		{Model: "no-canonical", Labs: []string{}, Status: "unmatched"},
		{Model: "missing", Labs: []string{}, Status: "unmatched"},
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

func TestModelsDevOwnershipRejectsMalformedCanonicalAssociation(t *testing.T) {
	for _, value := range []string{"", "model", "/model", "lab/", "lab /model", "lab/model\n", "lab/model\x00", "lab/*"} {
		if lab, valid := modelsDevCanonicalLab(value); valid {
			t.Fatalf("accepted canonical %q as %q", value, lab)
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
	for _, names := range [][]string{nil, {""}, {"family*"}, {strings.Repeat("a", 101)}, make([]string, 501)} {
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
	want := []ModelOwnershipSuggestion{{Model: "a", Labs: []string{"lab"}, Status: "matched"}, {Model: "z", Labs: []string{}, Status: "unmatched"}}
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
