package oidc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"one-api/common/config"
)

func TestGetOIDCConfigUsesCurrentRelevantOptions(t *testing.T) {
	issuerA := newTestIssuer(t, nil)
	issuerB := newTestIssuer(t, nil)
	manager := installOIDCTestOptions(t)

	publishOIDCTestOptions(t, manager, 1, issuerA.URL, "one")
	first, err := GetOIDCConfigInstance()
	if err != nil {
		t.Fatalf("build first OIDC config: %v", err)
	}

	// Unrelated configuration changes must not rebuild the derived OIDC client.
	publishOIDCTestOptions(t, manager, 2, issuerA.URL, "two")
	second, err := GetOIDCConfigInstance()
	if err != nil {
		t.Fatalf("get cached OIDC config: %v", err)
	}
	if second != first {
		t.Fatal("unrelated option change rebuilt OIDC config")
	}

	publishOIDCTestOptions(t, manager, 3, issuerB.URL, "three")
	third, err := GetOIDCConfigInstance()
	if err != nil {
		t.Fatalf("build updated OIDC config: %v", err)
	}
	if third == first {
		t.Fatal("relevant option change reused stale OIDC config")
	}
	if got, want := third.OAuth2Config.Endpoint.AuthURL, issuerB.URL+"/auth"; got != want {
		t.Fatalf("AuthURL = %q, want %q", got, want)
	}
}

func TestOIDCDiscoveryHonorsCallerDeadline(t *testing.T) {
	requestStarted := make(chan struct{})
	issuer := newTestIssuer(t, func(w http.ResponseWriter, r *http.Request) bool {
		close(requestStarted)
		<-r.Context().Done()
		return true
	})
	manager := installOIDCTestOptions(t)
	publishOIDCTestOptions(t, manager, 1, issuer.URL, "one")

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	startedAt := time.Now()
	if _, err := GetOIDCConfigInstanceWithContext(ctx); err == nil {
		t.Fatal("discovery unexpectedly succeeded after context deadline")
	}
	if elapsed := time.Since(startedAt); elapsed > time.Second {
		t.Fatalf("discovery ignored caller deadline: %v", elapsed)
	}
	select {
	case <-requestStarted:
	default:
		t.Fatal("discovery request was not started")
	}
}

func TestStaleOIDCDiscoveryDoesNotReplaceCurrentCache(t *testing.T) {
	discoveryStarted := make(chan struct{})
	releaseDiscovery := make(chan struct{})
	issuerA := newTestIssuer(t, func(http.ResponseWriter, *http.Request) bool {
		close(discoveryStarted)
		<-releaseDiscovery
		return false
	})
	issuerB := newTestIssuer(t, nil)
	manager := installOIDCTestOptions(t)
	publishOIDCTestOptions(t, manager, 1, issuerA.URL, "one")

	type result struct {
		config *OIDCConfig
		err    error
	}
	staleResult := make(chan result, 1)
	go func() {
		instance, err := GetOIDCConfigInstance()
		staleResult <- result{config: instance, err: err}
	}()
	<-discoveryStarted

	publishOIDCTestOptions(t, manager, 2, issuerB.URL, "two")
	current, err := GetOIDCConfigInstance()
	if err != nil {
		t.Fatalf("build current OIDC config: %v", err)
	}
	close(releaseDiscovery)
	stale := <-staleResult
	if stale.err != nil {
		t.Fatalf("finish stale OIDC discovery: %v", stale.err)
	}
	if got, want := stale.config.OAuth2Config.Endpoint.AuthURL, issuerA.URL+"/auth"; got != want {
		t.Fatalf("stale caller AuthURL = %q, want %q", got, want)
	}

	cached, err := GetOIDCConfigInstance()
	if err != nil {
		t.Fatalf("get current cached OIDC config: %v", err)
	}
	if cached != current {
		t.Fatal("stale discovery replaced the current OIDC cache")
	}
}

func newTestIssuer(t *testing.T, intercept func(http.ResponseWriter, *http.Request) bool) *httptest.Server {
	t.Helper()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if intercept != nil && intercept(w, r) {
			return
		}
		if r.URL.Path != "/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                server.URL,
			"authorization_endpoint":                server.URL + "/auth",
			"token_endpoint":                        server.URL + "/token",
			"jwks_uri":                              server.URL + "/keys",
			"response_types_supported":              []string{"code"},
			"subject_types_supported":               []string{"public"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	}))
	t.Cleanup(server.Close)
	return server
}

func installOIDCTestOptions(t *testing.T) *config.OptionManager {
	t.Helper()
	manager := config.NewOptionManager()
	enabled := false
	clientID, clientSecret, issuer, scopes, usernameClaim := "", "", "", "", ""
	serverAddress, unrelated := "", ""
	manager.RegisterBool("OIDCAuthEnabled", &enabled)
	manager.RegisterString("OIDCClientId", &clientID)
	manager.RegisterString("OIDCClientSecret", &clientSecret)
	manager.RegisterString("OIDCIssuer", &issuer)
	manager.RegisterString("OIDCScopes", &scopes)
	manager.RegisterString("OIDCUsernameClaims", &usernameClaim)
	manager.RegisterString("ServerAddress", &serverAddress)
	manager.RegisterString("Unrelated", &unrelated)

	original := config.GlobalOption
	config.GlobalOption = manager
	resetOIDCConfigCache()
	t.Cleanup(func() {
		resetOIDCConfigCache()
		config.GlobalOption = original
	})
	return manager
}

func publishOIDCTestOptions(t *testing.T, manager *config.OptionManager, version int64, issuer, unrelated string) {
	t.Helper()
	_, err := manager.PublishRuntimeOverrides(version, map[string]string{
		"OIDCAuthEnabled":    "true",
		"OIDCClientId":       "client-id",
		"OIDCClientSecret":   "client-secret",
		"OIDCIssuer":         issuer,
		"OIDCScopes":         "openid,profile",
		"OIDCUsernameClaims": "preferred_username",
		"ServerAddress":      "https://proxy.example",
		"Unrelated":          unrelated,
	})
	if err != nil {
		t.Fatalf("publish runtime options: %v", err)
	}
}

func resetOIDCConfigCache() {
	oidcConfigCache.Lock()
	oidcConfigCache.key = oidcConfigKey{}
	oidcConfigCache.instance = nil
	oidcConfigCache.Unlock()
}
