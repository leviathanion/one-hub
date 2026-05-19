package session

import (
	"fmt"
	"net/http"
	"strings"
)

const ClientSessionIDMaxLen = 128

func ReadClientSessionID(req *http.Request) string {
	if req == nil {
		return ""
	}
	if sessionID := strings.TrimSpace(req.Header.Get("x-session-id")); sessionID != "" {
		return sessionID
	}
	return strings.TrimSpace(req.Header.Get("session_id"))
}

func ValidateClientSessionID(sessionID string) error {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return fmt.Errorf("session_id must not be empty")
	}
	if len(sessionID) > ClientSessionIDMaxLen {
		return fmt.Errorf("session_id must be %d characters or fewer", ClientSessionIDMaxLen)
	}
	for _, r := range sessionID {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.', r == ':':
		default:
			return fmt.Errorf("session_id contains unsupported character %q", r)
		}
	}
	return nil
}
