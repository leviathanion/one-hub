package antigravity

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"one-api/common/config"
	"one-api/common/requester"
	"one-api/model"
	"one-api/providers/base"
	"one-api/types"
)

func modelDiscoveryProvider(t *testing.T, endpoint string) *AntigravityProvider {
	t.Helper()
	previousClient := requester.HTTPClient
	transport := http.DefaultTransport.(*http.Transport).Clone()
	requester.HTTPClient = &http.Client{Transport: transport}
	t.Cleanup(func() {
		requester.HTTPClient = previousClient
		transport.CloseIdleConnections()
	})
	creds := &Credentials{
		AccessToken: "model-discovery-access", RefreshToken: "model-discovery-refresh",
		ExpiresAt: time.Now().Add(time.Hour).Unix(), Subject: "model-discovery-subject", ProjectID: "model-discovery-project",
	}
	key, err := creds.ToJSON()
	if err != nil {
		t.Fatal(err)
	}
	return &AntigravityProvider{BaseProvider: base.BaseProvider{
		Channel:   &model.Channel{Type: config.ChannelTypeAntigravity, Key: key, BaseURL: &endpoint},
		Requester: requester.NewHTTPRequester("", nil),
	}}
}

func TestGetModelListUsesProjectAndPreservesDiscoveredIDs(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/v1internal:fetchAvailableModels" || r.URL.RawQuery != "" {
			t.Errorf("unexpected discovery request: %s %s", r.Method, r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer model-discovery-access" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("User-Agent") == "" {
			t.Error("discovery request lost provider headers")
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !reflect.DeepEqual(body, map[string]string{"project": "model-discovery-project"}) {
			t.Errorf("unexpected discovery body: %v, err=%v", body, err)
		}
		_, _ = io.WriteString(w, `{"models":{"Future-Model/V2":{"newCapability":[true,{"future":1}]},"gemini-custom":{"quotaInfo":{"remainingFraction":0}},"claude-new":null},"webSearchModelIds":["search-only-metadata"],"future":{"union":[1,"x"]}}`)
	}))
	defer server.Close()
	p := modelDiscoveryProvider(t, server.URL)
	beforeKey := p.Channel.Key
	models, err := p.GetModelList()
	want := []string{"Future-Model/V2", "claude-new", "gemini-custom"}
	if err != nil || !reflect.DeepEqual(models, want) || requests.Load() != 1 {
		t.Fatalf("models=%v err=%v requests=%d", models, err, requests.Load())
	}
	if p.Channel.Key != beforeKey || p.Channel.Models != "" {
		t.Fatal("model discovery mutated the draft credentials or configured models")
	}
}

func TestGetModelListDistinguishesEmptyCatalogFromMalformedResponse(t *testing.T) {
	for _, tc := range []struct {
		name  string
		body  string
		valid bool
	}{
		{name: "empty catalog", body: `{"models":{}}`, valid: true},
		{name: "missing models", body: `{"webSearchModelIds":[]}`},
		{name: "null models", body: `{"models":null}`},
		{name: "array models", body: `{"models":[]}`},
		{name: "truncated JSON", body: `{"models":`},
		{name: "trailing JSON", body: `{"models":{}} {}`},
		{name: "empty model ID", body: `{"models":{" ":{}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			models, err := modelDiscoveryProvider(t, server.URL).GetModelList()
			if tc.valid {
				if err != nil || models == nil || len(models) != 0 {
					t.Fatalf("empty catalog = %v, %v", models, err)
				}
			} else if err == nil || models != nil {
				t.Fatalf("malformed catalog = %v, %v", models, err)
			}
		})
	}
}

func TestGetModelListPreservesErrorStatusWithoutRetry(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"error":{"message":"model-discovery-access model-discovery-project","code":`+strconv.Itoa(status)+`}}`)
			}))
			defer server.Close()
			models, err := modelDiscoveryProvider(t, server.URL).GetModelList()
			var apiErr *types.OpenAIErrorWithStatusCode
			if !errors.As(err, &apiErr) || apiErr.StatusCode != status || models != nil || requests.Load() != 1 {
				t.Fatalf("models=%v err=%v requests=%d", models, err, requests.Load())
			}
			if strings.Contains(err.Error(), "model-discovery-access") || strings.Contains(err.Error(), "model-discovery-project") {
				t.Fatal("model discovery exposed upstream identity details")
			}
		})
	}
}

func TestGetModelListRejectsRedirectWithoutFollowing(t *testing.T) {
	var followed atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		followed.Add(1)
		_, _ = io.WriteString(w, `{"models":{"unexpected":{}}}`)
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	models, err := modelDiscoveryProvider(t, source.URL).GetModelList()
	if err == nil || models != nil || followed.Load() != 0 {
		t.Fatalf("models=%v err=%v followed=%d", models, err, followed.Load())
	}
}

func TestGetModelListBoundsCatalogBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"models":{"future":{"padding":"`+strings.Repeat("x", controlBodyLimit)+`"}}}`)
	}))
	defer server.Close()
	models, err := modelDiscoveryProvider(t, server.URL).GetModelList()
	if err == nil || models != nil || !strings.Contains(err.Error(), "4 MiB") {
		t.Fatalf("oversized catalog = %v, %v", models, err)
	}
}

func TestGetModelListRejectsExpiredDraftBeforeDispatch(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = io.WriteString(w, `{"models":{}}`)
	}))
	defer server.Close()
	p := modelDiscoveryProvider(t, server.URL)
	creds, err := ParseCredentials(p.Channel.Key)
	if err != nil {
		t.Fatal(err)
	}
	creds.ExpiresAt = 1
	p.Channel.Key, err = creds.ToJSON()
	if err != nil {
		t.Fatal(err)
	}
	models, err := p.GetModelList()
	if err == nil || models != nil || requests.Load() != 0 {
		t.Fatalf("expired draft: models=%v err=%v requests=%d", models, err, requests.Load())
	}
}
