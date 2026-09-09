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

func TestPreservedRedirectRedactsOnlyCredentialRepresentations(t *testing.T) {
	for _, bodyHasCredential := range []bool{false, true} {
		for _, location := range []string{"https://example.test/safe", "https://example.test/provider-secret-123", "https://example.test/%70rovider-secret-123"} {
			t.Run(location+"/body="+map[bool]string{false: "safe", true: "secret"}[bodyHasCredential], func(t *testing.T) {
				body := "redirect body\n"
				if bodyHasCredential {
					body += "provider-secret-123"
				}
				req, _ := http.NewRequest(http.MethodPost, "https://upstream.test", nil)
				req.Header.Set("Authorization", "Bearer provider-secret-123")
				resp := &http.Response{StatusCode: 307, Request: req, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{
					"Location": {location}, "Content-Length": {"42"}, "Digest": {"original"}, "Content-Type": {"text/plain"},
				}}
				apiErr := preservedRedirectResponse(resp, providerresponse.OperationUnknown)
				if apiErr.StatusCode != 307 || !apiErr.ReplayRawResponse || strings.Contains(string(apiErr.RawBody), "provider-secret-123") {
					t.Fatalf("redirect semantics/security: %+v", apiErr)
				}
				if !bodyHasCredential && string(apiErr.RawBody) != body {
					t.Fatalf("safe body changed: %q", apiErr.RawBody)
				}
				if (apiErr.ResponseHeaders.Get("Content-Length") == "") != bodyHasCredential || (apiErr.ResponseHeaders.Get("Digest") == "") != bodyHasCredential {
					t.Fatalf("representation headers mismatch: %v", apiErr.ResponseHeaders)
				}
				wantLocation := ""
				if location == "https://example.test/safe" {
					wantLocation = location
				}
				if apiErr.ResponseHeaders.Get("Location") != wantLocation {
					t.Fatalf("redirect target changed/leaked: %v", apiErr.ResponseHeaders)
				}
			})
		}
	}
}
