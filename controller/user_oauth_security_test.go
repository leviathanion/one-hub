package controller

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
	jose "github.com/go-jose/go-jose/v4"
	"net/http"
	"net/http/httptest"
	"one-api/common/config"
	"one-api/model"
	"testing"
	"time"
)

func TestUserSecurityGitHubEmptyEmailDoesNotSelectLocalAccount(t *testing.T) {
	u := userGroupControllerFixture(t)
	if err := model.DB.Model(&model.User{}).Where("id = ?", u.Id).Update("role", config.RoleRootUser).Error; err != nil {
		t.Fatal(err)
	}
	got, err := getUserByGitHub(&GitHubUser{Id: 987654, Login: "unrelated-github", Email: ""})
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("无关联 GitHub 的空邮箱匹配本地用户: id=%d role=%d email=%q", got.Id, got.Role, got.Email)
	}
}
func TestUserSecurityGitHubStableIDMismatchRejectsNameFallback(t *testing.T) {
	u := userGroupControllerFixture(t)
	if err := model.DB.Model(&model.User{}).Where("id = ?", u.Id).Updates(map[string]any{"github_id": "reused-name", "github_id_new": 123, "email": "owner@example.com"}).Error; err != nil {
		t.Fatal(err)
	}
	value := false
	config.GlobalOption.RegisterBool("GitHubOldIdCloseEnabled", &value)
	if _, err := config.GlobalOption.PublishRuntimeOverrides(2, map[string]string{"QuotaPerUnit": "1", "GitHubOldIdCloseEnabled": "false"}); err != nil {
		t.Fatal(err)
	}
	got, err := getUserByGitHub(&GitHubUser{Id: 456, Login: "reused-name", Email: "different@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("来访 GitHub ID=456 却匹配已绑定 ID=%d 的本地用户=%d", got.GitHubIdNew, got.Id)
	}
}
func TestUserSecurityOIDCInviteRewards(t *testing.T) {
	userGroupControllerFixture(t)
	if err := model.DB.Model(&model.User{}).Where("id = ?", 1).Update("aff_code", "review-invite").Error; err != nil {
		t.Fatal(err)
	}
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: private}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "review"))
	if err != nil {
		t.Fatal(err)
	}
	var issuer *httptest.Server
	issuer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]any{"issuer": issuer.URL, "authorization_endpoint": issuer.URL + "/auth", "token_endpoint": issuer.URL + "/token", "jwks_uri": issuer.URL + "/keys", "response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"}, "id_token_signing_alg_values_supported": []string{"RS256"}})
		case "/keys":
			_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &private.PublicKey, KeyID: "review", Algorithm: "RS256", Use: "sig"}}})
		case "/token":
			claims, _ := json.Marshal(map[string]any{"iss": issuer.URL, "aud": "review-client", "sub": "new-subject", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "preferred_username": "oidc-new"})
			signed, e := signer.Sign(claims)
			if e != nil {
				http.Error(w, e.Error(), 500)
				return
			}
			token, e := signed.CompactSerialize()
			if e != nil {
				http.Error(w, e.Error(), 500)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "fixture-access", "token_type": "Bearer", "expires_in": 3600, "id_token": token})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(issuer.Close)
	manager := config.GlobalOption
	for _, key := range []string{"OIDCAuthEnabled", "RegisterEnabled"} {
		v := true
		manager.RegisterBool(key, &v)
	}
	for _, key := range []string{"OIDCIssuer", "OIDCClientId", "OIDCClientSecret", "OIDCScopes", "OIDCUsernameClaims", "ServerAddress"} {
		v := ""
		manager.RegisterString(key, &v)
	}
	for _, key := range []string{"QuotaForNewUser", "QuotaForInvitee", "QuotaForInviter"} {
		v := 0
		manager.RegisterInt(key, &v)
	}
	if _, err := manager.PublishRuntimeOverrides(2, map[string]string{"QuotaPerUnit": "1", "OIDCAuthEnabled": "true", "RegisterEnabled": "true", "OIDCIssuer": issuer.URL, "OIDCClientId": "review-client", "OIDCClientSecret": "review-secret", "OIDCScopes": "openid profile email", "OIDCUsernameClaims": "preferred_username", "ServerAddress": "https://hub.example", "QuotaForNewUser": "0", "QuotaForInvitee": "50", "QuotaForInviter": "25"}); err != nil {
		t.Fatal(err)
	}
	if err := model.DB.Create(&model.Option{Key: "OIDCIssuer", Value: issuer.URL}).Error; err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	router.Use(sessions.Sessions("session", cookie.NewStore([]byte("oidc-review-session-secret"))))
	router.GET("/seed", func(c *gin.Context) {
		s := sessions.Default(c)
		s.Set("oauth_state", "review-state")
		if err := s.Save(); err != nil {
			t.Fatal(err)
		}
		c.Status(200)
	})
	router.GET("/oauth/oidc", OIDCAuth)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/seed", nil))
	cookies := w.Result().Cookies()
	req := httptest.NewRequest(http.MethodGet, "/oauth/oidc?state=review-state&code=fixture&aff=review-invite", nil)
	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	var response struct{ Success bool }
	_ = json.Unmarshal(w.Body.Bytes(), &response)
	if !response.Success {
		t.Fatalf("OIDC fixture 未完成注册: %s", w.Body.String())
	}
	var invitee model.User
	if err := model.DB.Where("username = ?", "oidc-new").First(&invitee).Error; err != nil {
		t.Fatal(err)
	}
	inviter, err := model.GetUserById(1, false)
	if err != nil {
		t.Fatal(err)
	}
	if invitee.InviterId != 1 {
		t.Fatal("邀请码未识别，场景无效")
	}
	if invitee.Quota != 50 || inviter.Quota != 225 {
		t.Fatalf("OIDC 已记录 inviter_id=%d，但邀请奖励未入账: invitee=%d want50 inviter=%d want225", invitee.InviterId, invitee.Quota, inviter.Quota)
	}
}
