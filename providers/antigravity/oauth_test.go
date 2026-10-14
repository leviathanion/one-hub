package antigravity

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"one-api/common/requester"
)

func oauthResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func mockOAuthClient(t *testing.T, do func(*http.Request) (*http.Response, error)) *OAuthClient {
	t.Helper()
	c, err := NewOAuthClient("")
	if err != nil {
		t.Fatal(err)
	}
	c.do, c.pollInterval = do, 0
	return c
}

func TestAuthorizationIncludesPKCEAndFixedRedirect(t *testing.T) {
	c := mockOAuthClient(t, nil)
	u, err := url.Parse(c.AuthorizationURL("session", "challenge"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Scheme+"://"+u.Host+u.Path != AuthEndpoint || q.Get("state") != "session" || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") != "challenge" || q.Get("redirect_uri") != c.config.RedirectURI || q.Has("client_secret") {
		t.Fatal("invalid authorization URL")
	}
}

func TestCodeExchangeGetsTrustedSubjectAndProject(t *testing.T) {
	var calls []string
	c := mockOAuthClient(t, func(req *http.Request) (*http.Response, error) {
		calls = append(calls, req.URL.String())
		switch req.URL.String() {
		case TokenEndpoint:
			if err := req.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if req.Form.Get("code") != "code" || req.Form.Get("code_verifier") != "verifier" {
				t.Fatal("lost OAuth session parameters")
			}
			return oauthResponse(200, `{"access_token":"opaque-token","refresh_token":"refresh","expires_in":3600,"token_type":"Bearer"}`), nil
		case UserInfoEndpoint:
			if req.Header.Get("Authorization") != "Bearer opaque-token" {
				t.Fatal("missing bearer")
			}
			return oauthResponse(200, `{"id":"stable-id","email":"display@example.test"}`), nil
		case projectBaseURL + "/v1internal:loadCodeAssist":
			return oauthResponse(200, `{"cloudaicompanionProject":{"id":"project"}}`), nil
		default:
			t.Fatalf("unexpected URL: %s", req.URL.Path)
			return nil, nil
		}
	})
	creds, err := c.ExchangeCode(t.Context(), "code", "verifier", c.config.RedirectURI)
	if err != nil || creds.Subject != "stable-id" || creds.ProjectID != "project" || len(calls) != 3 || creds.ExpiresAt <= time.Now().Unix() {
		t.Fatalf("exchange failed: %v", err)
	}
}

func TestRefreshRetainsIdentityAndMissingRefreshTokenWithoutDiscovery(t *testing.T) {
	calls := 0
	c := mockOAuthClient(t, func(req *http.Request) (*http.Response, error) {
		calls++
		if req.URL.String() != TokenEndpoint {
			t.Fatal("runtime refresh performed discovery")
		}
		return oauthResponse(200, `{"access_token":"next","expires_in":3600}`), nil
	})
	previous := &Credentials{AccessToken: "old", RefreshToken: "old-refresh", ExpiresAt: 1, Subject: "subject", ProjectID: "project", Email: "display"}
	next, err := c.Refresh(t.Context(), previous)
	if err != nil || calls != 1 || next.RefreshToken != previous.RefreshToken || next.Subject != previous.Subject || next.ProjectID != previous.ProjectID || previous.AccessToken != "old" {
		t.Fatalf("refresh = %v", err)
	}
}

func TestProjectOnboardingSubmitsOnceAndObservesReturnedOperation(t *testing.T) {
	onboards, observations := 0, 0
	c := mockOAuthClient(t, func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/v1internal:loadCodeAssist":
			return oauthResponse(200, `{"allowedTiers":[{"id":"default-tier","isDefault":true}]}`), nil
		case "/v1internal:onboardUser":
			if req.URL.String() != DefaultBaseURL+"/v1internal:onboardUser" {
				t.Fatal("onboarding used an unexpected origin")
			}
			onboards++
			return oauthResponse(200, `{"name":"operations/operation-1","done":false}`), nil
		case "/v1internal/operations/operation-1":
			if req.URL.String() != DefaultBaseURL+"/v1internal/operations/operation-1" {
				t.Fatal("operation observation changed origin")
			}
			observations++
			if req.Method != http.MethodGet {
				t.Fatal("operation was resubmitted")
			}
			return oauthResponse(200, `{"done":true,"response":{"cloudaicompanionProject":"project"}}`), nil
		default:
			t.Fatal("unexpected operation")
			return nil, nil
		}
	})
	project, err := c.initializeProject(t.Context(), "token")
	if err != nil || project != "project" || onboards != 1 || observations != 1 {
		t.Fatalf("project=%q error=%v submissions=%d observations=%d", project, err, onboards, observations)
	}
}

func TestProjectInitializationDoesNotInventProjectOrReplayOnboarding(t *testing.T) {
	for _, reply := range []string{`{"done":true,"response":{}}`, `{"done":false}`, `{"done":false,"name":"operations/../token"}`, `{"error":{"message":"private-project"}}`} {
		calls := 0
		c := mockOAuthClient(t, func(req *http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				return oauthResponse(200, `{"allowedTiers":[{"id":"tier","isDefault":true}]}`), nil
			}
			return oauthResponse(200, reply), nil
		})
		if project, err := c.initializeProject(t.Context(), "token"); err == nil || project != "" || calls != 2 || strings.Contains(err.Error(), "private-project") {
			t.Fatalf("unexpected project initialization outcome: %d %q %v", calls, project, err)
		}
	}
}

func TestOAuthMalformedOversizedAndRejectedResponsesRemainDispatched(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"malformed", 200, `{"access_token":`}, {"missing_expiry", 200, `{"access_token":"private-token"}`},
		{"too_large", 200, strings.Repeat(" ", oauthBodyLimit+1)}, {"rejected", 401, `{"error":"private-refresh-token"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			c := mockOAuthClient(t, func(*http.Request) (*http.Response, error) { calls++; return oauthResponse(tc.status, tc.body), nil })
			_, err := c.Refresh(t.Context(), &Credentials{RefreshToken: "refresh"})
			if err == nil || errors.Is(err, ErrOAuthRefreshNotDispatched) || calls != 1 || strings.Contains(err.Error(), "private-") {
				t.Fatalf("refresh error=%v calls=%d", err, calls)
			}
		})
	}
	calls := 0
	c := mockOAuthClient(t, func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("private proxy failure") })
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := c.Refresh(ctx, &Credentials{RefreshToken: "refresh"}); !errors.Is(err, ErrOAuthRefreshNotDispatched) || calls != 0 {
		t.Fatal("canceled refresh was dispatched")
	}
}

type oauthRoundTripper func(*http.Request) (*http.Response, error)

func (f oauthRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestOAuthBearerRedirectIsNeverFollowed(t *testing.T) {
	old := requester.HTTPClient
	t.Cleanup(func() { requester.HTTPClient = old })
	calls := 0
	requester.HTTPClient = &http.Client{Transport: oauthRoundTripper(func(req *http.Request) (*http.Response, error) {
		calls++
		resp := oauthResponse(302, `{}`)
		resp.Header.Set("Location", "https://untrusted.example.test/token")
		return resp, nil
	})}
	c, err := NewOAuthClient("")
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	err = c.readJSON(t.Context(), http.MethodGet, UserInfoEndpoint, "private-token", "", nil, oauthBodyLimit, &result)
	if err == nil || calls != 1 {
		t.Fatalf("redirect calls=%d err=%v", calls, err)
	}
}

func TestAuthorizationRequiresTrustedSubjectInsteadOfEmail(t *testing.T) {
	calls := 0
	c := mockOAuthClient(t, func(req *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			if err := req.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if req.Form.Get("grant_type") != "authorization_code" || req.Form.Get("code") != "authorized" {
				t.Fatal("invalid authorization grant")
			}
			return oauthResponse(200, `{"access_token":"opaque","expires_in":3600}`), nil
		}
		if req.URL.String() != UserInfoEndpoint {
			t.Fatal("project initialization started without subject")
		}
		return oauthResponse(200, `{"email":"display-only@example.test"}`), nil
	})
	if result, err := c.ExchangeCode(t.Context(), "authorized", "verifier", c.config.RedirectURI); err == nil || result != nil || calls != 2 {
		t.Fatal("email was accepted as stable subject")
	}
}

func TestProjectOperationObservationIsBounded(t *testing.T) {
	onboards, observations := 0, 0
	c := mockOAuthClient(t, func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/v1internal:loadCodeAssist":
			return oauthResponse(200, `{"allowedTiers":[{"id":"tier","isDefault":true}]}`), nil
		case "/v1internal:onboardUser":
			onboards++
		default:
			observations++
		}
		return oauthResponse(200, `{"name":"operations/pending","done":false}`), nil
	})
	if project, err := c.initializeProject(t.Context(), "token"); err == nil || project != "" || onboards != 1 || observations != 10 {
		t.Fatalf("project=%q err=%v onboarding=%d observations=%d", project, err, onboards, observations)
	}
}
