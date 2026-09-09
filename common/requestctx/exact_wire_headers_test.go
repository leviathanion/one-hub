package requestctx

import (
	"net/http"
	"reflect"
	"testing"
)

func TestApplyRegisteredExactWireRequestHeaders(t *testing.T) {
	inbound := NewHeaderSnapshot(http.Header{
		"Idempotency-Key":                    {"idem-1"},
		"If-None-Match":                      {`"etag-1"`, `"etag-2"`},
		"If-Modified-Since":                  {"Mon, 17 Aug 2026 00:00:00 GMT"},
		"X-Future-Business":                  {"one", "two"},
		"X-Admin-Fixed":                      {"client"},
		"Authorization":                      {"Bearer client-secret"},
		"Api-Key":                            {"client-secret"},
		"Cookie":                             {"session=secret"},
		"Host":                               {"client.example"},
		"Content-Length":                     {"999"},
		"Connection":                         {"keep-alive, X-Connection-Private"},
		"X-Connection-Private":               {"secret"},
		"X-One-Hub-Channel-Id":               {"7"},
		"Accept-Encoding":                    {"gzip"},
		"X-Forwarded-Access-Token":           {"identity.jwt.secret"},
		"Cf-Access-Jwt-Assertion":            {"identity.jwt.secret"},
		"Cf-Access-Authenticated-User-Email": {"user@example.com"},
		"Cf-Access-Client-Secret":            {"service-secret"},
		"X-Ms-Token-Aad-Access-Token":        {"easy-auth-access-token"},
		"X-Ms-Client-Principal":              {"easy-auth-principal"},
		"X-Ms-Client-Principal-Id":           {"easy-auth-principal-id"},
		"X-Goog-Authenticated-User-Email":    {"accounts.google.com:user@example.com"},
		"X-Goog-Authenticated-User-Id":       {"accounts.google.com:123456"},
		"X-Pomerium-Jwt-Assertion":           {"pomerium.jwt.secret"},
		"X-Vouch-Token":                      {"vouch.jwt.secret"},
		"X-Original-Authorization":           {"Bearer original-secret"},
	})
	dst := http.Header{
		"Authorization": {"Bearer provider-secret"},
		"X-Admin-Fixed": {"admin"},
	}
	if err := ApplyRegisteredExactWireRequestHeaders(dst, inbound); err != nil {
		t.Fatalf("apply headers: %v", err)
	}
	if got := dst.Get("Authorization"); got != "Bearer provider-secret" {
		t.Fatalf("provider authorization changed: %q", got)
	}
	if got := dst.Get("X-Admin-Fixed"); got != "admin" {
		t.Fatalf("administrator header lost priority: %q", got)
	}
	if got := dst.Get("Idempotency-Key"); got != "idem-1" {
		t.Fatalf("idempotency key=%q", got)
	}
	if got := dst.Get("If-Modified-Since"); got == "" {
		t.Fatal("conditional request header was not forwarded")
	}
	if got := dst.Values("If-None-Match"); !reflect.DeepEqual(got, []string{`"etag-1"`, `"etag-2"`}) {
		t.Fatalf("conditional multi-values=%v", got)
	}
	if got := dst.Values("X-Future-Business"); !reflect.DeepEqual(got, []string{"one", "two"}) {
		t.Fatalf("unknown business header values=%v", got)
	}
	for _, blocked := range []string{"Api-Key", "Cookie", "Host", "Content-Length", "Accept-Encoding", "Connection", "X-Connection-Private", "X-One-Hub-Channel-Id", "X-Forwarded-Access-Token", "Cf-Access-Jwt-Assertion", "Cf-Access-Authenticated-User-Email", "Cf-Access-Client-Secret", "X-Ms-Token-Aad-Access-Token", "X-Ms-Client-Principal", "X-Ms-Client-Principal-Id", "X-Goog-Authenticated-User-Email", "X-Goog-Authenticated-User-Id", "X-Pomerium-Jwt-Assertion", "X-Vouch-Token", "X-Original-Authorization"} {
		if got := dst.Values(blocked); len(got) != 0 {
			t.Fatalf("blocked header %s leaked: %v", blocked, got)
		}
	}
}

func TestApplyRegisteredExactWireRequestHeadersRejectsInvalidValue(t *testing.T) {
	inbound := HeaderSnapshot{Fields: map[string]HeaderField{
		"x-invalid": {CanonicalName: "X-Invalid", Values: []string{"safe\r\nInjected: yes"}},
	}}
	if err := ApplyRegisteredExactWireRequestHeaders(make(http.Header), inbound); err == nil {
		t.Fatal("expected CRLF header value to be rejected")
	}
}

func TestConfiguredIngressOwnedHeaderIsBlocked(t *testing.T) {
	ConfigureExactWireOwnedRequestHeaders([]string{"X-Company-Identity", "invalid header"})
	t.Cleanup(func() { ConfigureExactWireOwnedRequestHeaders(nil) })
	dst := make(http.Header)
	inbound := NewHeaderSnapshot(http.Header{
		"X-Company-Identity": {"internal-user"},
		"X-Business-Field":   {"kept"},
	})
	if err := ApplyRegisteredExactWireRequestHeaders(dst, inbound); err != nil {
		t.Fatalf("apply headers: %v", err)
	}
	if dst.Get("X-Company-Identity") != "" || dst.Get("X-Business-Field") != "kept" {
		t.Fatalf("deployment header registry applied incorrectly: %v", dst)
	}
}
