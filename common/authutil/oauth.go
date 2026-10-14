package authutil

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/url"
	"strings"
)

func GeneratePKCEVerifier() (string, error) {
	b := make([]byte, 64)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func PKCEChallenge(verifier string) string {
	hash := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(hash[:])
}

// ParseOAuthCallback 的空 redirect 保留调用方原有的 query-only 回调策略。
// 提供 redirect 时，只接受该完整回调地址和无歧义的参数。
func ParseOAuthCallback(input, expectedState, expectedRedirect string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(input))
	if err != nil {
		return "", errors.New("invalid callback URL")
	}
	if expectedRedirect != "" {
		redirect, err := url.Parse(expectedRedirect)
		if err != nil || redirect.Scheme == "" || redirect.Host == "" || u.Scheme != redirect.Scheme || u.Host != redirect.Host || u.EscapedPath() != redirect.EscapedPath() || u.User != nil || u.Fragment != "" || redirect.RawQuery != "" || redirect.Fragment != "" {
			return "", errors.New("callback URL does not match OAuth redirect")
		}
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", errors.New("invalid callback query")
	}
	if expectedRedirect != "" {
		for _, values := range query {
			if len(values) != 1 {
				return "", errors.New("callback query contains duplicate parameters")
			}
		}
	}
	states := query["state"]
	if len(states) != 1 || states[0] != expectedState {
		return "", errors.New("callback state does not match OAuth session")
	}
	if _, denied := query["error"]; denied {
		return "", errors.New("OAuth authorization was denied")
	}
	codes := query["code"]
	if len(codes) != 1 || codes[0] == "" {
		return "", errors.New("callback URL must contain one authorization code")
	}
	return codes[0], nil
}
