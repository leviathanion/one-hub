package kling

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

type klingRoundTripFunc func(*http.Request) (*http.Response, error)

func (f klingRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestKlingSubmitPassesCallerDeadlineToHTTPRequest(t *testing.T) {
	proxy := ""
	provider := KlingProviderFactory{}.Create(&model.Channel{Key: "access|secret", Proxy: &proxy}).(*KlingProvider)
	originalClient := requester.HTTPClient
	var observed time.Time
	requester.HTTPClient = &http.Client{Transport: klingRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		observed, _ = req.Context().Deadline()
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":0,"data":{"task_id":"task-1"}}`)), Request: req}, nil
	})}
	t.Cleanup(func() { requester.HTTPClient = originalClient })

	deadline := time.Now().Add(2 * time.Minute).Round(0)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	_, _ = provider.Submit(ctx, "videos", "text2video", &KlingTask{})
	if observed.IsZero() || !observed.Equal(deadline) {
		t.Fatalf("provider request deadline=%v want %v", observed, deadline)
	}
}
