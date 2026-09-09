package suno

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"one-api/common/requester"
	"one-api/model"
)

type sunoRoundTripFunc func(*http.Request) (*http.Response, error)

func (f sunoRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestSunoSubmitPassesCallerDeadlineToHTTPRequest(t *testing.T) {
	proxy, baseURL := "", "https://suno.example"
	provider := SunoProviderFactory{}.Create(&model.Channel{Key: "key", Proxy: &proxy, BaseURL: &baseURL}).(*SunoProvider)
	originalClient := requester.HTTPClient
	var observed time.Time
	requester.HTTPClient = &http.Client{Transport: sunoRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		observed, _ = req.Context().Deadline()
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":"success","data":"task-1"}`)), Request: req}, nil
	})}
	t.Cleanup(func() { requester.HTTPClient = originalClient })

	deadline := time.Now().Add(2 * time.Minute).Round(0)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	_, _ = provider.Submit(ctx, SunoActionMusic, &SunoSubmitReq{})
	if observed.IsZero() || !observed.Equal(deadline) {
		t.Fatalf("provider request deadline=%v want %v", observed, deadline)
	}
}
