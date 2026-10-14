package controller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"one-api/common/config"
	commonTest "one-api/common/test"
	"one-api/model"
	"one-api/providers/antigravity"

	"github.com/gin-gonic/gin"
)

func antigravityPrettyCredential(t *testing.T, accessToken string) string {
	t.Helper()
	raw, err := json.MarshalIndent(&antigravity.Credentials{
		AccessToken: accessToken, RefreshToken: "refresh-token", ExpiresAt: time.Now().Add(time.Hour).Unix(),
		Subject: "google-subject", ProjectID: "project-one",
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestAntigravityChannelCreateKeepsWholeCredential(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useControllerChannelTagTestDB(t)
	key := antigravityPrettyCredential(t, "access-token")
	channel := model.Channel{Type: config.ChannelTypeAntigravity, Name: "google", Key: key, Models: "model-one", Group: "default"}
	channels := buildChannelsForCreate(channel)
	if len(channels) != 1 || channels[0].Key != key {
		t.Fatal("pretty JSON must be one complete credential")
	}
	body, _ := json.Marshal(channel)
	c, recorder := commonTest.GetContext(http.MethodPost, "/api/channel", commonTest.RequestJSONConfig(), bytes.NewBuffer(body))
	AddChannel(c)
	var result codexOAuthAPIResult
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil || !result.Success {
		t.Fatalf("valid credential was rejected: %s", recorder.Body.String())
	}
	var persisted []model.Channel
	if err := model.DB.Find(&persisted).Error; err != nil {
		t.Fatal(err)
	}
	if len(persisted) != 1 || strings.Contains(persisted[0].Key, "\n") {
		t.Fatalf("expected one compact credential, got %d channels", len(persisted))
	}
	if _, err := antigravity.ParseCredentials(persisted[0].Key); err != nil {
		t.Fatal(err)
	}
}

func TestAntigravityChannelCreateRejectsIncompleteCredentialWithoutWrites(t *testing.T) {
	for _, key := range []string{"", "{", `{"refresh_token":"refresh-only"}`, `{"access_token":"token","refresh_token":"refresh","expires_at":1,"subject":"subject"}`} {
		t.Run(key, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			useControllerChannelTagTestDB(t)
			body, _ := json.Marshal(model.Channel{Type: config.ChannelTypeAntigravity, Key: key, Name: "incomplete"})
			c, recorder := commonTest.GetContext(http.MethodPost, "/api/channel", commonTest.RequestJSONConfig(), bytes.NewBuffer(body))
			AddChannel(c)
			var result codexOAuthAPIResult
			if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			var count int64
			if err := model.DB.Model(&model.Channel{}).Count(&count).Error; err != nil {
				t.Fatal(err)
			}
			if result.Success || count != 0 {
				t.Fatal("incomplete credential must not become a schedulable channel")
			}
		})
	}
}

func TestAntigravityChannelEditValidatesSubmittedCredentialOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useControllerChannelTagTestDB(t)
	channel := model.Channel{Id: 72, Type: config.ChannelTypeAntigravity, Key: antigravityPrettyCredential(t, "old-access"), Name: "original", Models: "model-one", Group: "default"}
	if err := model.DB.Create(&channel).Error; err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []bool{true, false} {
		payload := map[string]any{"id": channel.Id, "expected_version": channel.Version, "name": "renamed"}
		if invalid {
			payload["key"] = `{"refresh_token":"not-complete"}`
		}
		body, _ := json.Marshal(payload)
		c, recorder := commonTest.GetContext(http.MethodPut, "/api/channel", commonTest.RequestJSONConfig(), bytes.NewBuffer(body))
		UpdateChannel(c)
		var result codexOAuthAPIResult
		if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		persisted, err := model.GetChannelById(channel.Id)
		if err != nil {
			t.Fatal(err)
		}
		if invalid {
			if result.Success || persisted.Name != channel.Name || persisted.Version != channel.Version {
				t.Fatal("invalid credential must reject the entire edit")
			}
		} else if !result.Success || persisted.Name != "renamed" || persisted.Key != channel.Key {
			t.Fatalf("metadata edit must preserve the omitted credential: %s", result.Message)
		}
	}
}

func TestAntigravityTagAddNormalizesWholeCredentialAndRejectsDuplicates(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useControllerChannelTagTestDB(t)
	channel := model.Channel{Id: 73, Type: config.ChannelTypeAntigravity, Key: antigravityPrettyCredential(t, "old-access"), Name: "first", Tag: "google-tag", Models: "model-one", Group: "default"}
	if err := model.DB.Create(&channel).Error; err != nil {
		t.Fatal(err)
	}
	newKey := antigravityPrettyCredential(t, "new-access")
	for _, key := range []string{newKey, newKey, `{"refresh_token":"incomplete"}`} {
		body, _ := json.Marshal(map[string]string{"key": key, "name": "second"})
		c, recorder := commonTest.GetContext(http.MethodPost, "/api/channel_tag/google-tag", commonTest.RequestJSONConfig(), bytes.NewBuffer(body))
		c.Params = gin.Params{{Key: "tag", Value: "google-tag"}}
		AddChannelToTag(c)
		var result codexOAuthAPIResult
		if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		var count int64
		if err := model.DB.Model(&model.Channel{}).Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 2 {
			t.Fatalf("only one valid new credential may be added; count=%d, message=%s", count, result.Message)
		}
	}
}

func TestAntigravityModelDiscoverySeparatesDraftAndPersistedCredentials(t *testing.T) {
	for _, scenario := range []string{"pretty_json_draft", "expired_draft", "persisted", "persisted_pretty_key"} {
		t.Run(scenario, func(t *testing.T) {
			server := setupAntigravityOAuthTest(t)
			channel := antigravityOAuthChannelFixture(t, false)
			payload := map[string]any{"id": channel.Id, "type": channel.Type}
			if scenario == "persisted_pretty_key" {
				credential, err := antigravity.ParseCredentials(channel.Key)
				if err != nil {
					t.Fatal(err)
				}
				credential.ExpiresAt = time.Now().Add(-time.Minute).Unix()
				channel.Key, err = credential.ToJSON()
				if err != nil {
					t.Fatal(err)
				}
				if err := model.DB.Model(&model.Channel{}).Where("id = ?", channel.Id).UpdateColumn("key", channel.Key).Error; err != nil {
					t.Fatal(err)
				}
				var pretty bytes.Buffer
				if err := json.Indent(&pretty, []byte(channel.Key), "", "  "); err != nil {
					t.Fatal(err)
				}
				payload["key"] = pretty.String()
			} else if scenario != "persisted" {
				key := antigravityPrettyCredential(t, "draft-access")
				if scenario == "expired_draft" {
					credential, err := antigravity.ParseCredentials(key)
					if err != nil {
						t.Fatal(err)
					}
					credential.ExpiresAt = time.Now().Add(-time.Minute).Unix()
					key, err = credential.ToJSON()
					if err != nil {
						t.Fatal(err)
					}
				}
				payload["key"] = key
			}
			body, _ := json.Marshal(payload)
			c, recorder := commonTest.GetContext(http.MethodPost, "/api/channel/provider_models_list", commonTest.RequestJSONConfig(), bytes.NewBuffer(body))
			GetModelList(c)
			var result codexOAuthAPIResult
			if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if scenario == "expired_draft" {
				if result.Success || server.modelCalls.Load() != 0 || server.tokenCalls.Load() != 0 {
					t.Fatal("expired draft must fail without OAuth refresh or model discovery")
				}
			} else {
				want := "Bearer draft-access"
				if scenario == "persisted" {
					want = "Bearer old-access"
				} else if scenario == "persisted_pretty_key" {
					want = "Bearer new-access"
				}
				if !result.Success || server.modelCalls.Load() != 1 || server.modelAuth != want {
					t.Fatalf("wrong discovery credential: success=%v calls=%d auth=%q message=%s", result.Success, server.modelCalls.Load(), server.modelAuth, result.Message)
				}
			}
			persisted, err := model.GetChannelById(channel.Id)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "persisted_pretty_key" {
				if server.tokenCalls.Load() != 1 || persisted.Key == channel.Key || persisted.Version != channel.Version+2 {
					t.Fatal("unchanged pretty JSON must retain the persisted refresh fence")
				}
			} else if persisted.Key != channel.Key || persisted.Version != channel.Version {
				t.Fatal("model discovery must not write draft credentials")
			}
		})
	}
}
