package session

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReadClientSessionIDPrefersExplicitHeader(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/realtime", nil)
	req.Header.Set("x-session-id", "execution-session-456")
	req.Header.Set("session-id", "native-session")
	req.Header.Set("session_id", "legacy-session")

	if got := ReadClientSessionID(req); got != "execution-session-456" {
		t.Fatalf("expected x-session-id to win, got %q", got)
	}
}

func TestReadClientSessionIDPrefersNativeSessionIDOverLegacyHeader(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/realtime", nil)
	req.Header.Set("session-id", "native-session")
	req.Header.Set("session_id", "legacy-session")

	if got := ReadClientSessionID(req); got != "native-session" {
		t.Fatalf("expected session-id to win over session_id, got %q", got)
	}
}

func TestReadClientSessionIDFallsBackToNativeSessionID(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/realtime", nil)
	req.Header.Set("Session-Id", "native-session")

	if got := ReadClientSessionID(req); got != "native-session" {
		t.Fatalf("expected session-id fallback, got %q", got)
	}
}

func TestReadClientSessionIDFallsBackToLegacyHeader(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/realtime", nil)
	req.Header.Set("session_id", "legacy-session")

	if got := ReadClientSessionID(req); got != "legacy-session" {
		t.Fatalf("expected session_id fallback, got %q", got)
	}
}

func TestReadClientSessionIDHandlesNilRequest(t *testing.T) {
	if got := ReadClientSessionID(nil); got != "" {
		t.Fatalf("expected nil request to yield empty client session id, got %q", got)
	}
}
