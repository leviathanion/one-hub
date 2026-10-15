package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"one-api/common/config"
	"one-api/common/requester"
	"one-api/model"
	"one-api/providers/antigravity"
	"one-api/providers/gemini"
)

type fixtureTransport struct {
	body  string
	calls int
}

func (t *fixtureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.calls++
	if req.URL.Host != "offline.invalid" || req.URL.Path != "/v1internal:streamGenerateContent" {
		return nil, errors.New("unexpected upstream operation")
	}
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(t.body)), Request: req}, nil
}

// Exercise the actual provider, requester framer and native SSE rewrite. The
// transport returns a fixture and never opens a network connection.
func nativeSSE(input string) result {
	transport := &fixtureTransport{body: input}
	previous := requester.HTTPClient
	requester.HTTPClient = &http.Client{Transport: transport}
	defer func() { requester.HTTPClient = previous }()
	creds := &antigravity.Credentials{AccessToken: "fixture-access", RefreshToken: "fixture-refresh", Subject: "fixture-subject", ProjectID: "fixture-project", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	key, err := creds.ToJSON()
	if err != nil {
		return result{Error: err.Error()}
	}
	endpoint := "http://offline.invalid"
	p := (antigravity.AntigravityProviderFactory{}).Create(&model.Channel{Type: config.ChannelTypeAntigravity, Key: key, BaseURL: &endpoint}).(*antigravity.AntigravityProvider)
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c.Request = httptest.NewRequest(http.MethodPost, "/client-entry", strings.NewReader(`{"contents":[]}`)).WithContext(ctx)
	p.SetContext(c)
	stream, apiErr := p.CreateGeminiChatStream(&gemini.NativeRequest{Raw: []byte(`{"contents":[]}`), Model: "fixture-model", Stream: true})
	if apiErr != nil {
		return result{Error: apiErr.Error()}
	}
	defer requester.CloseAndDrainStream(stream)
	data, failures := stream.Recv()
	out := result{}
	for data != nil || failures != nil {
		select {
		case frame, ok := <-data:
			if !ok {
				data = nil
			} else {
				out.Frames = append(out.Frames, frame)
			}
		case failure, ok := <-failures:
			if !ok {
				failures = nil
			} else if failure != nil && !errors.Is(failure, io.EOF) {
				out.Error = failure.Error()
			}
		case <-ctx.Done():
			return result{Error: ctx.Err().Error()}
		}
	}
	if transport.calls != 1 {
		out.Error = "unexpected dispatch count"
	}
	return out
}
