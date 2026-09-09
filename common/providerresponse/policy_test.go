package providerresponse

import (
	"net/http"
	"testing"
)

func TestFilterUsesDenyByDefaultBasePolicy(t *testing.T) {
	headers := http.Header{
		"Apim-Request-Id":              []string{"azure-req-123"},
		"Content-Type":                 []string{"application/json"},
		"Retry-After":                  []string{"3"},
		"X-Request-Id":                 []string{"req_123"},
		"Set-Cookie":                   []string{"session=provider"},
		"Openai-Organization":          []string{"org_shared"},
		"X-Ratelimit-Remaining-Tokens": []string{"42"},
		"X-Future-Debug-Header":        []string{"secret"},
	}

	got := Filter(headers, Policy{})
	if got.Get("Retry-After") != "3" || got.Get("X-Request-Id") != "req_123" || got.Get("Apim-Request-Id") != "azure-req-123" {
		t.Fatalf("expected safe base headers, got %#v", got)
	}
	if got.Get("Content-Type") != "" {
		t.Fatalf("transformed response must not inherit provider content type, got %#v", got)
	}
	for _, forbidden := range []string{"Set-Cookie", "Openai-Organization", "X-Ratelimit-Remaining-Tokens", "X-Future-Debug-Header"} {
		if got.Get(forbidden) != "" {
			t.Fatalf("expected %s to be denied, got %#v", forbidden, got)
		}
	}
}

func TestFilterAddsOnlyOperationSpecificRepresentationHeaders(t *testing.T) {
	headers := http.Header{
		"Content-Type":        []string{"application/json"},
		"Content-Encoding":    []string{"gzip"},
		"Content-Length":      []string{"10"},
		"Content-Language":    []string{"en"},
		"Cache-Control":       []string{"private, no-store"},
		"Vary":                []string{"Origin, Accept-Encoding"},
		"Expires":             []string{"0"},
		"Pragma":              []string{"no-cache"},
		"Digest":              []string{"sha-256=:YWJj:"},
		"Age":                 []string{"5"},
		"Content-Disposition": []string{`attachment; filename="result.bin"`},
		"Content-Range":       []string{"bytes 0-9/10"},
		"Etag":                []string{`"version-1"`},
		"Last-Modified":       []string{"Sat, 15 Aug 2026 00:00:00 GMT"},
		"Location":            []string{"https://example.test/result"},
	}

	raw := Filter(headers, Policy{Operation: OperationRawRelay, DataPath: DataPathExactWire, BodyUnmodified: true, PreserveRedirect: true})
	for name := range headers {
		if raw.Get(name) == "" {
			t.Fatalf("expected raw relay to preserve %s, got %#v", name, raw)
		}
	}

	retrieve := Filter(headers, Policy{Operation: OperationResponsesRetrieve, DataPath: DataPathExactWire, BodyUnmodified: true})
	for _, allowed := range []string{"Content-Type", "Content-Encoding", "Content-Length", "Content-Language", "Cache-Control", "Vary", "Expires", "Pragma", "Digest", "Age", "Etag", "Last-Modified"} {
		if retrieve.Get(allowed) == "" {
			t.Fatalf("expected retrieve to preserve %s, got %#v", allowed, retrieve)
		}
	}
	for _, denied := range []string{"Content-Disposition", "Content-Range", "Location"} {
		if retrieve.Get(denied) != "" {
			t.Fatalf("expected retrieve to deny %s, got %#v", denied, retrieve)
		}
	}

	decoded := Filter(headers, Policy{Operation: OperationRawRelay, DataPath: DataPathCrossProtocol})
	if len(decoded) != 0 {
		t.Fatalf("expected transformed response to deny representation headers, got %#v", decoded)
	}
}

func TestFilterDropsRepresentationHeadersAfterRewrite(t *testing.T) {
	headers := http.Header{
		"Content-Type":     []string{"application/json"},
		"Content-Encoding": []string{"gzip"},
		"Content-Length":   []string{"42"},
		"Cache-Control":    []string{"private"},
		"Etag":             []string{`"encoded-representation"`},
		"Digest":           []string{"sha-256=:YWJj:"},
		"Content-Range":    []string{"bytes 0-9/42"},
	}

	got := Filter(headers, Policy{Operation: OperationRawRelay, DataPath: DataPathExactWire, BodyUnmodified: false})
	for name := range headers {
		if value := got.Get(name); value != "" {
			t.Fatalf("rewritten representation retained %s=%q", name, value)
		}
	}
}
