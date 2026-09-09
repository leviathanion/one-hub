package controller

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"one-api/common/cache"
	"one-api/common/config"
	commonTest "one-api/common/test"
	"one-api/model"
	"one-api/providers/codex"
)

func TestCodexOAuthReauthorizationPreservesAccountAndCredentialVersion(t *testing.T) {
	for _, scenario := range []string{"same-account", "existing-fence", "different-account", "concurrent-refresh", "new-fence", "exchange-error"} {
		t.Run(scenario, func(t *testing.T) {
			useControllerChannelTagTestDB(t)
			cache.InitCacheManager()
			key := `{"access_token":"old-access","refresh_token":"old-refresh","account_id":"account-a"}`
			channel := model.Channel{Type: config.ChannelTypeCodex, Key: key}
			if scenario == "existing-fence" {
				fence := "unresolved-at-start"
				channel.CredentialRefreshFence = &fence
			}
			if err := model.DB.Create(&channel).Error; err != nil {
				t.Fatal(err)
			}
			startBody, _ := json.Marshal(map[string]any{"channel_id": channel.Id})
			ctx, recorder := commonTest.GetContext(http.MethodPost, "/api/codex/oauth/start", commonTest.RequestJSONConfig(), bytes.NewBuffer(startBody))
			StartCodexOAuth(ctx)
			var start struct {
				Success bool `json:"success"`
				Data    struct {
					SessionID string `json:"session_id"`
				} `json:"data"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &start); err != nil || !start.Success {
				t.Fatalf("start failed: %s err=%v", recorder.Body.String(), err)
			}
			if scenario == "concurrent-refresh" || scenario == "new-fence" {
				ticket := model.CredentialRotationTicket{ChannelID: channel.Id, AttemptID: "later-refresh", ExpectedRevision: 0}
				if outcome, err := model.ClaimCredentialRotation(context.Background(), ticket, time.Now()); err != nil || outcome != model.CredentialRotationClaimAcquired {
					t.Fatalf("claim=%v err=%v", outcome, err)
				}
				if scenario == "concurrent-refresh" {
					if outcome, err := model.CommitCredentialRotation(context.Background(), ticket, "winning-refresh"); err != nil || outcome != model.CredentialRotationCommitApplied {
						t.Fatalf("commit=%v err=%v", outcome, err)
					}
				}
			}
			before, err := model.LoadCredentialRotationSnapshot(context.Background(), channel.Id)
			if err != nil {
				t.Fatal(err)
			}
			accountID := "account-a"
			if scenario == "different-account" {
				accountID = "account-b"
			}
			claims, _ := json.Marshal(map[string]any{"https://api.openai.com/auth": map[string]string{"chatgpt_account_id": accountID}})
			token := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256"}`)) + "." + base64.RawURLEncoding.EncodeToString(claims) + ".signature"
			var exchangeCalls atomic.Int32
			withTokenEndpointTLSServer(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				exchangeCalls.Add(1)
				claimed, err := model.LoadCredentialRotationSnapshot(context.Background(), channel.Id)
				if err != nil || claimed.Fence == nil || claimed.Revision != 0 {
					t.Errorf("OAuth exchange started without SQL claim: %+v err=%v", claimed, err)
				}
				peer := model.CredentialRotationTicket{ChannelID: channel.Id, ExpectedRevision: 0, AttemptID: "competing-refresh"}
				if outcome, err := model.ClaimCredentialRotation(context.Background(), peer, time.Now()); err != nil || outcome != model.CredentialRotationClaimBusy {
					t.Errorf("refresh was not fenced during OAuth exchange: outcome=%v err=%v", outcome, err)
				}
				if scenario == "exchange-error" {
					writer.WriteHeader(http.StatusBadGateway)
					return
				}
				_ = json.NewEncoder(writer).Encode(map[string]any{"access_token": token, "refresh_token": "authorized-refresh", "expires_in": 3600})
			}))
			callbackBody, _ := json.Marshal(map[string]any{"session_id": start.Data.SessionID, "authorization_code": "authorization-code-for-test-123456"})
			ctx, recorder = commonTest.GetContext(http.MethodPost, "/api/codex/oauth/exchange-code", commonTest.RequestJSONConfig(), bytes.NewBuffer(callbackBody))
			CodexOAuthCallback(ctx)
			var result struct {
				Success bool `json:"success"`
				Data    struct {
					Saved bool `json:"credential_saved"`
				} `json:"data"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			after, err := model.LoadCredentialRotationSnapshot(context.Background(), channel.Id)
			if err != nil {
				t.Fatal(err)
			}
			wantCalls := int32(1)
			if scenario == "concurrent-refresh" || scenario == "new-fence" {
				wantCalls = 0
			}
			if got := exchangeCalls.Load(); got != wantCalls {
				t.Fatalf("OAuth calls=%d want=%d; response=%s", got, wantCalls, recorder.Body.String())
			}

			if scenario == "same-account" || scenario == "existing-fence" {
				credentials, err := codex.FromJSON(after.Key)
				if !result.Success || !result.Data.Saved || err != nil || after.Revision != before.Revision+1 || after.Fence != nil || credentials.AccountID != "account-a" || credentials.AccessToken != token {
					t.Fatalf("reauthorization failed: response=%s snapshot=%+v err=%v", recorder.Body.String(), after, err)
				}
			} else if result.Success || after.Key != before.Key || after.Revision != before.Revision || (after.Fence == nil) != (before.Fence == nil) {
				t.Fatalf("stale/different account replaced credentials: response=%s before=%+v after=%+v", recorder.Body.String(), before, after)
			}
		})
	}
}
