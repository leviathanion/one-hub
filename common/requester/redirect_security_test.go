package requester

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"one-api/common/providerresponse"
)

type failingRedirectReader struct{}

func (failingRedirectReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

type oversizedRedirectReader struct{}

func (oversizedRedirectReader) Read(p []byte) (int, error) { return len(p), nil }

func TestRedirectBodyFailurePreservesExecutionFact(t *testing.T) {
	for _, reader := range []io.Reader{failingRedirectReader{}, io.LimitReader(oversizedRedirectReader{}, maxProviderRawJSONBodyBytes+1)} {
		apiErr := preservedRedirectResponse(&http.Response{StatusCode: 307, Body: io.NopCloser(reader)}, providerresponse.OperationUnknown)
		if apiErr == nil || !apiErr.UpstreamAccepted || !apiErr.LocalError || apiErr.ReplayRawResponse || apiErr.StatusCode != http.StatusBadGateway {
			t.Fatalf("local failure lost execution fact: %+v", apiErr)
		}
		if !strings.Contains(apiErr.Message, "exceeds") && !strings.Contains(apiErr.Message, io.ErrUnexpectedEOF.Error()) {
			t.Fatalf("unexpected read failure: %+v", apiErr)
		}
	}
}

func TestPreservedRedirectDoesNotRunBodyRedaction(t *testing.T) {
	for _, body := range []string{"redirect provider-secret", `{"text":"provider\u002dsecret","future":9007199254740993}`, `{"text":"one","text":"two"}`, `{"text":`} {
		req, _ := http.NewRequest(http.MethodPost, "https://upstream.test", nil)
		req.Header.Set("Authorization", "Bearer provider-secret")
		location := "https://example.test/provider-secret"
		resp := &http.Response{StatusCode: 307, Request: req, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{"Location": {location}, "Digest": {"original"}, "Content-Type": {"application/json"}}}
		apiErr := preservedRedirectResponse(resp, providerresponse.OperationUnknown)
		if apiErr.StatusCode != 307 || !apiErr.ReplayRawResponse || string(apiErr.RawBody) != body || apiErr.ResponseHeaders.Get("Location") != location || apiErr.ResponseHeaders.Get("Digest") != "original" {
			t.Fatalf("脱敏干扰重定向: %+v", apiErr)
		}
	}
}
