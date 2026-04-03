package session

import (
	"net/http"
	"strings"
)

func ReadClientSessionID(req *http.Request) string {
	if req == nil {
		return ""
	}
	if sessionID := strings.TrimSpace(req.Header.Get("x-session-id")); sessionID != "" {
		return sessionID
	}
	return strings.TrimSpace(req.Header.Get("session_id"))
}
