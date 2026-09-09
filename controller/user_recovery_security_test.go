package controller

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
	jose "github.com/go-jose/go-jose/v4"
	"net/http"
	"net/http/httptest"
	"one-api/common"
	"one-api/common/config"
	"one-api/model"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestUserSecurityOIDCBoundSubjectCannotBeReplaced(t *testing.T) {
	userGroupControllerFixture(t)
	if err := model.DB.Model(&model.User{}).Where("id = ?", 1).Updates(map[string]any{"aff_code": "review-invite", "oidc_id": "existing-subject", "role": config.RoleRootUser}).Error; err != nil {
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
			claims, _ := json.Marshal(map[string]any{"iss": issuer.URL, "aud": "review-client", "sub": "new-subject", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "preferred_username": "group-user"})
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
	current, err := model.GetUserById(1, false)
	if err != nil {
		t.Fatal(err)
	}
	if response.Success || current.OidcId != "existing-subject" {
		t.Fatalf("不同 subject 经同名回退登录并改绑: success=%v role=%d oidc_id=%s", response.Success, current.Role, current.OidcId)
	}
}

func TestUserSecurityEmailCodeCannotBindMultipleUsers(t *testing.T) {
	userGroupControllerFixture(t)
	second := &model.User{Username: "second-user", Password: "password123", AccessToken: "second-access", AffCode: "second-aff"}
	if err := model.DB.Create(second).Error; err != nil {
		t.Fatal(err)
	}
	const email = "shared-review@example.com"
	const code = "a1b2c3"
	if err := model.StoreUserVerification(context.Background(), email, common.EmailVerificationPurpose, code, 0); err != nil {
		t.Fatal(err)
	}

	successes := 0
	for _, id := range []int{1, second.Id} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Set("id", id)
		c.Request = httptest.NewRequest(http.MethodGet, "/oauth/email/bind?email="+email+"&code="+code, nil)
		EmailBind(c)
		var response struct{ Success bool }
		_ = json.Unmarshal(w.Body.Bytes(), &response)
		if response.Success {
			successes++
		}
	}
	// 即使另一请求持有新签发的有效码，也不能制造第二个邮箱归属。
	if err := model.StoreUserVerification(context.Background(), email, common.EmailVerificationPurpose, "another-valid-code", 0); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("id", second.Id)
	c.Request = httptest.NewRequest(http.MethodGet, "/oauth/email/bind?email="+email+"&code=another-valid-code", nil)
	EmailBind(c)
	var repeated struct{ Success bool }
	if err := json.Unmarshal(w.Body.Bytes(), &repeated); err != nil {
		t.Fatal(err)
	}
	if repeated.Success {
		t.Fatal("新验证码绕过邮箱唯一归属")
	}
	var count int64
	if err := model.DB.Model(&model.User{}).Where("email = ?", email).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if err := model.ResetUserPassword(context.Background(), 1, email, "shared-reset-password"); err != nil {
		t.Fatal(err)
	}
	var users []model.User
	if err := model.DB.Where("email = ?", email).Find(&users).Error; err != nil {
		t.Fatal(err)
	}
	changed := 0
	for _, user := range users {
		if common.ValidatePasswordAndHash("shared-reset-password", user.Password) {
			changed++
		}
	}
	if count != 1 {
		t.Fatalf("同一验证码顺序重复绑定成功: successes=%d 邮箱归属数=%d 同时被重置密码的账号=%d", successes, count, changed)
	}
}
func TestUserSecurityPasswordResetTokenConsumedOnce(t *testing.T) {
	userGroupControllerFixture(t)
	const email = "reset-review@example.com"
	const code = "reset-link-token"
	if err := model.SetUserEmail(1, email); err != nil {
		t.Fatal(err)
	}
	if err := model.StoreUserVerification(context.Background(), email, common.PasswordResetPurpose, code, 1); err != nil {
		t.Fatal(err)
	}

	type result struct {
		Success bool
		Data    string
	}
	start := make(chan struct{})
	outputs := make(chan result, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/api/user/reset", strings.NewReader(`{"email":"`+email+`","token":"`+code+`"}`))
			ResetPassword(c)
			var response result
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Error(err)
			}
			outputs <- response
		}()
	}
	close(start)
	wg.Wait()
	close(outputs)
	user, err := model.GetUserById(1, true)
	if err != nil {
		t.Fatal(err)
	}
	successes, usable := 0, 0
	for result := range outputs {
		if result.Success {
			successes++
			if common.ValidatePasswordAndHash(result.Data, user.Password) {
				usable++
			}
		}
	}
	if successes != 1 {
		t.Fatalf("同一重置 token 被消费 %d 次，返回密码中仅 %d 个与最终数据库一致", successes, usable)
	}
}
