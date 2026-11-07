package antigravity

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"one-api/common/requester"
)

func TestUsageSnapshotUsesModelCatalogQuotaWithoutGuessingMissingValues(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/v1internal:fetchAvailableModels" || r.Header.Get("Authorization") != "Bearer model-discovery-access" {
			t.Errorf("unexpected quota request: %s %s", r.Method, r.URL)
		}
		var request map[string]string
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || !reflect.DeepEqual(request, map[string]string{"project": "model-discovery-project"}) {
			t.Errorf("quota request project lost: %v err=%v", request, err)
		}
		_, _ = io.WriteString(w, `{"models":{
			"gemini-3.8-flash-high":{"displayName":"Gemini Flash","quotaInfo":{"remainingFraction":0,"resetTime":"2026-11-08T12:34:56Z"}},
			"future-model":{"quotaInfo":{"remainingFraction":1}},
			"missing-quota":{},
			"null-entry":null,
			"partial-quota":{"quotaInfo":{"resetTime":"2026-11-08T12:34:56+08:00"}},
			"invalid-quota":{"quotaInfo":{"remainingFraction":1.5,"resetTime":"not-a-time"}}
		},"access_token":"model-discovery-access","subject":"model-discovery-subject","project":"model-discovery-project"}`)
	}))
	defer server.Close()
	p := modelDiscoveryProvider(t, server.URL)
	before := time.Now().Unix()
	snapshot, err := p.GetUsageSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ChannelID != p.Channel.Id || snapshot.FetchedAt < before || snapshot.FetchedAt > time.Now().Unix() || requests.Load() != 1 {
		t.Fatalf("invalid snapshot metadata: %+v requests=%d", snapshot, requests.Load())
	}
	wantIDs := []string{"future-model", "gemini-3.8-flash-high", "invalid-quota", "missing-quota", "null-entry", "partial-quota"}
	ids := make([]string, 0, len(snapshot.Models))
	for _, quota := range snapshot.Models {
		ids = append(ids, quota.ModelID)
		switch quota.ModelID {
		case "future-model":
			if quota.RemainingFraction == nil || *quota.RemainingFraction != 1 || quota.ResetTime != "" {
				t.Fatalf("full quota changed: %+v", quota)
			}
		case "gemini-3.8-flash-high":
			if quota.RemainingFraction == nil || *quota.RemainingFraction != 0 || quota.DisplayName != "Gemini Flash" || quota.ResetTime != "2026-11-08T12:34:56Z" {
				t.Fatalf("zero quota or reset time lost: %+v", quota)
			}
		case "partial-quota":
			if quota.RemainingFraction != nil || quota.ResetTime != "2026-11-08T12:34:56+08:00" {
				t.Fatalf("partial quota changed: %+v", quota)
			}
		default:
			if quota.RemainingFraction != nil || quota.ResetTime != "" {
				t.Fatalf("unknown quota guessed: %+v", quota)
			}
		}
	}
	if !reflect.DeepEqual(ids, wantIDs) {
		t.Fatalf("models=%v want=%v", ids, wantIDs)
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"model-discovery-access", "model-discovery-subject", "model-discovery-project", "quotaInfo", "access_token"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("snapshot exposed provider metadata: %s", secret)
		}
	}
}

func TestUsageSnapshotInvalidFractionIsUnknown(t *testing.T) {
	for _, value := range []string{`null`, `-0.1`, `1.01`, `"0.5"`, `true`, `{}`, `[]`, `1e999`} {
		t.Run(value, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `{"models":{"dynamic-model":{"quotaInfo":{"remainingFraction":`+value+`}}}}`)
			}))
			defer server.Close()
			snapshot, err := modelDiscoveryProvider(t, server.URL).GetUsageSnapshot(context.Background())
			if err != nil || len(snapshot.Models) != 1 || snapshot.Models[0].RemainingFraction != nil {
				t.Fatalf("invalid fraction should remain unknown: snapshot=%+v err=%v", snapshot, err)
			}
		})
	}
}

func TestUsageSnapshotRejectsFailedCatalogWithoutRetry(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"upstream error", http.StatusForbidden, `{"error":{"message":"model-discovery-access model-discovery-project"}}`},
		{"missing models", http.StatusOK, `{"secret":"model-discovery-access"}`},
		{"malformed catalog", http.StatusOK, `{"models":`},
		{"oversized catalog", http.StatusOK, `{"models":{"model":{"padding":"` + strings.Repeat("x", controlBodyLimit) + `"}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			snapshot, err := modelDiscoveryProvider(t, server.URL).GetUsageSnapshot(context.Background())
			if err == nil || snapshot != nil || requests.Load() != 1 {
				t.Fatalf("failed catalog: snapshot=%+v err=%v requests=%d", snapshot, err, requests.Load())
			}
			if strings.Contains(err.Error(), "model-discovery-access") || strings.Contains(err.Error(), "model-discovery-project") {
				t.Fatalf("error leaked credentials: %v", err)
			}
		})
	}
}

func TestUsageSnapshotHonorsCanceledContext(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = io.WriteString(w, `{"models":{}}`)
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	snapshot, err := modelDiscoveryProvider(t, server.URL).GetUsageSnapshot(ctx)
	if err == nil || snapshot != nil || requests.Load() != 0 {
		t.Fatalf("canceled quota query: snapshot=%+v err=%v requests=%d", snapshot, err, requests.Load())
	}
}

func TestUsageSnapshotRedactsConstructionAndReadFailures(t *testing.T) {
	t.Run("request URL", func(t *testing.T) {
		p := modelDiscoveryProvider(t, "http://%model-discovery-access/model-discovery-project")
		snapshot, err := p.GetUsageSnapshot(context.Background())
		if err == nil || snapshot != nil {
			t.Fatalf("invalid URL accepted: snapshot=%+v err=%v", snapshot, err)
		}
		for _, secret := range []string{"model-discovery-access", "model-discovery-project"} {
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("request error exposed credential: %v", err)
			}
		}
	})
	t.Run("body read", func(t *testing.T) {
		p := modelDiscoveryProvider(t, "http://quota.invalid")
		requester.HTTPClient.Transport = oauthRoundTripper(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(antigravityFailingReader{err: errors.New("model-discovery-access model-discovery-project")}),
				Request:    req,
			}, nil
		})
		snapshot, err := p.GetUsageSnapshot(context.Background())
		if err == nil || snapshot != nil {
			t.Fatalf("unreadable body accepted: snapshot=%+v err=%v", snapshot, err)
		}
		for _, secret := range []string{"model-discovery-access", "model-discovery-project"} {
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("read error exposed credential: %v", err)
			}
		}
	})
}
