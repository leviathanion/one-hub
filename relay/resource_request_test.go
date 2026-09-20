package relay

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"one-api/common/config"
	"one-api/common/logger"
	"one-api/common/requestctx"
	"one-api/middleware"
	"one-api/model"
	"one-api/relay/relay_util"
	"one-api/types"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

func resourceRequestContext(raw []byte, userID int) *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(raw))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("id", userID)
	c.Set("token_id", 1)
	c.Set("token_setting", &model.TokenSetting{ResourceChannelID: 999})
	return c
}

func seedRequestResource(t *testing.T, kind, id string, userID, channelID int) *model.ResourceOwner {
	t.Helper()
	owner := &model.ResourceOwner{Kind: kind, UpstreamID: &id, UserID: userID, TokenID: 99, ChannelID: channelID, ProviderNamespace: "openai", ProviderScope: fmt.Sprintf("channel:%d", channelID), Phase: model.ResourceOwnerBound}
	if err := model.DB.Create(owner).Error; err != nil {
		t.Fatal(err)
	}
	return owner
}

func resourceRequestErrorCode(t *testing.T, err error) any {
	t.Helper()
	var apiErr *types.OpenAIErrorWithStatusCode
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected structured local error, got %v", err)
	}
	return apiErr.Code
}

func TestResourceRequestOwnerRoutingPreservesRawEnvelope(t *testing.T) {
	setupRelayTestDB(t, &model.ResourceOwner{})
	seedRequestResource(t, "file", "file_a", 1, 7)
	seedRequestResource(t, "conversation", "conv_a", 1, 7)
	for _, test := range []struct{ name, operation, body string }{
		{"responses file", "responses", "{ \"input\" : [{\"role\":\"user\",\"content\":[{\"type\":\"input_file\",\"file_id\":\"file_a\"}]}],\"unknown\": {\"number\":1.00e+9,\"union\":[true,{}]} }"},
		{"conversation string", "responses", `{"conversation":"conv_a","input":"hello","future":{"x":1}}`},
		{"conversation object and file", "responses", `{"conversation":{"id":"conv_a","future":7},"input":[{"type":"input_file","file_id":"file_a"}]}`},
		{"chat file", "chat", `{"messages":[{"role":"user","content":[{"type":"file","file":{"file_id":"file_a","future":true}}]}]}`},
		{"image file and mask", "images", `{"images":[{"file_id":"file_a","future":true}],"mask":{"file_id":"file_a"}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := []byte(test.body)
			before := bytes.Clone(raw)
			c := resourceRequestContext(raw, 1)
			if err := prepareResourceRequest(c, raw, test.operation); err != nil {
				t.Fatal(err)
			}
			if c.GetInt(relay_util.ResourceOwnerChannelKey) != 7 || c.GetInt("specific_channel_id") != 7 {
				t.Fatalf("owner route not selected: %+v", c.Keys)
			}
			if !bytes.Equal(raw, before) {
				t.Fatalf("observer rewrote bytes: %s", raw)
			}
			unread, err := io.ReadAll(c.Request.Body)
			if err != nil || !bytes.Equal(unread, before) {
				t.Fatalf("observer consumed original body: %s, %v", unread, err)
			}
			// The final effective body uses the same shared policy, including inserted refs.
			if err := requestctx.AuthorizeResourceReference(c, "file", "file_missing"); err == nil {
				t.Fatal("shared final-body policy did not reject an injected unknown reference")
			}
		})
	}
}

func TestResourceRequestRejectsOwnerConflictsAndUnauthorizedReferences(t *testing.T) {
	setupRelayTestDB(t, &model.ResourceOwner{})
	seedRequestResource(t, "file", "file_a", 1, 7)
	seedRequestResource(t, "file", "file_b", 1, 8)
	seedRequestResource(t, "conversation", "conv_other", 2, 9)
	for _, test := range []struct {
		name, body, code string
		preset           int
	}{
		{"mixed channels", `{"input":[{"type":"input_file","file_id":"file_a"},{"type":"input_file","file_id":"file_b"}]}`, "resource_channel_conflict", 0},
		{"another user", `{"conversation":"conv_other"}`, "resource_not_found", 0},
		{"unknown", `{"input":[{"type":"input_file","file_id":"file_missing"}]}`, "resource_not_found", 0},
		{"explicit pin conflict", `{"input":[{"type":"input_file","file_id":"file_a"}]}`, "resource_channel_conflict", 8},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := resourceRequestContext([]byte(test.body), 1)
			if test.preset > 0 {
				c.Set("specific_channel_id", test.preset)
			}
			if code := resourceRequestErrorCode(t, prepareResourceRequest(c, []byte(test.body), "responses")); code != test.code {
				t.Fatalf("code=%v want=%s", code, test.code)
			}
		})
	}
}

func TestResourceRequestSameIDRemainsUserScoped(t *testing.T) {
	setupRelayTestDB(t, &model.ResourceOwner{})
	seedRequestResource(t, "file", "file_shared_id", 1, 7)
	seedRequestResource(t, "file", "file_shared_id", 2, 8)
	raw := []byte(`{"input":[{"type":"input_file","file_id":"file_shared_id"}]}`)
	for userID, channelID := range map[int]int{1: 7, 2: 8} {
		c := resourceRequestContext(raw, userID)
		if err := prepareResourceRequest(c, raw, "responses"); err != nil {
			t.Fatal(err)
		}
		if got := c.GetInt(relay_util.ResourceOwnerChannelKey); got != channelID {
			t.Fatalf("user %d got route %d, want %d", userID, got, channelID)
		}
	}
}

func TestResourceRequestLocalRetentionIsSeparateFromUpstreamObservations(t *testing.T) {
	db := setupRelayTestDB(t, &model.ResourceOwner{})
	owner := seedRequestResource(t, "file", "file_deleted", 1, 7)
	past, future := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	if err := db.Model(owner).Updates(map[string]any{"delete_observed_at": past, "upstream_expires_at": past, "retain_until": future}).Error; err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"input":[{"type":"input_file","file_id":"file_deleted"}]}`)
	c := resourceRequestContext(raw, 1)
	if err := prepareResourceRequest(c, raw, "responses"); err != nil {
		t.Fatalf("upstream deletion/expiry must not override retained authorization: %v", err)
	}
	if err := db.Model(owner).Update("retain_until", past).Error; err != nil {
		t.Fatal(err)
	}
	c = resourceRequestContext(raw, 1)
	if code := resourceRequestErrorCode(t, prepareResourceRequest(c, raw, "responses")); code != "resource_not_found" {
		t.Fatalf("expired local proof accepted: %v", code)
	}
}

func TestResourceRequestOpaqueSchemaAndBusinessDataPassWithoutOwnerSQL(t *testing.T) {
	db := setupRelayTestDB(t, &model.ResourceOwner{})
	if err := db.Migrator().DropTable(&model.ResourceOwner{}); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{"input":[{"type":"tool_search_output","tools":[{"type":"function","parameters":{"properties":{"file_id":{"type":"string"}},"file_id":"business"}}]}]}`,
		`{"input":[{"type":"function_call_output","output":{"type":"input_file","file_id":"business"}}],"future":{"file_id":"business"}}`,
		`{"tools":[{"type":"function","parameters":{"file_id":"business"}}],"messages":[{"role":"user","content":"file_id"}]}`,
	} {
		c := resourceRequestContext([]byte(body), 1)
		if err := prepareResourceRequest(c, []byte(body), "responses"); err != nil {
			t.Fatalf("opaque business data triggered owner lookup: %v", err)
		}
		if c.GetInt(relay_util.ResourceOwnerChannelKey) != 0 {
			t.Fatal("no refs must not select the token resource default for inference")
		}
	}
	raw := []byte(`{"input":[{"type":"input_file","file_id":"file_a"}]}`)
	if code := resourceRequestErrorCode(t, prepareResourceRequest(resourceRequestContext(raw, 1), raw, "responses")); code != "resource_owner_unavailable" {
		t.Fatalf("SQL error silently granted authorization: %v", code)
	}
}

func TestResourceRequestDoesNotInterpretOtherDialectsExtensionFields(t *testing.T) {
	setupRelayTestDB(t, &model.ResourceOwner{})
	for _, operation := range []string{"chat", "images"} {
		raw := []byte(`{"input":[{"type":"input_file","file_id":"business"}],"conversation":{"id":"business"},"future":{"file_id":"business"}}`)
		c := resourceRequestContext(raw, 1)
		if err := prepareResourceRequest(c, raw, operation); err != nil {
			t.Fatalf("%s interpreted another dialect's extension as a resource: %v", operation, err)
		}
		if c.GetInt(relay_util.ResourceOwnerChannelKey) != 0 {
			t.Fatalf("%s extension chose a channel", operation)
		}
	}
}

func setupResourceNewWorkTest(t *testing.T) (*gorm.DB, *gin.Context) {
	t.Helper()
	db := setupRelayTestDB(t, &model.ResourceOwner{}, &model.User{}, &model.Token{}, &model.Channel{}, &model.PublicationVersion{})
	previousLogger := logger.Logger
	logger.Logger = zap.NewNop()
	t.Cleanup(func() { logger.Logger = previousLogger })
	snapshot := snapshotChannelGroup()
	t.Cleanup(func() { restoreChannelGroup(snapshot) })
	enabled := true
	if err := db.Create(&model.UserGroup{Symbol: "default", Ratio: 1, Enable: &enabled}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.User{Id: 1, Username: "resource-owner", Group: "default", Role: config.RoleCommonUser, Status: config.UserStatusEnabled}).Error; err != nil {
		t.Fatal(err)
	}
	token := &model.Token{Id: 1, UserId: 1, Key: "resource-route-token", Status: config.TokenStatusEnabled, ExpiredTime: -1}
	token.Setting.Set(model.TokenSetting{ResourceChannelID: 8, Limits: model.LimitsConfig{LimitModelSetting: model.LimitModelSetting{Enabled: true, Models: []string{"gpt-5"}}}})
	if err := db.Session(&gorm.Session{SkipHooks: true}).Create(token).Error; err != nil {
		t.Fatal(err)
	}
	if err := model.EnsurePublicationVersionRows(db); err != nil {
		t.Fatal(err)
	}
	if err := model.GlobalUserGroupRatio.Load(); err != nil {
		t.Fatal(err)
	}
	proxy := ""
	channel := &model.Channel{Id: 7, Name: "owner", Type: config.ChannelTypeOpenAI, Status: config.ChannelStatusEnabled, Group: "default", Models: "gpt-5", Key: "sk-test", Proxy: &proxy}
	if err := db.Create(channel).Error; err != nil {
		t.Fatal(err)
	}
	model.ChannelGroup = model.ChannelsChooser{Channels: map[int]*model.ChannelChoice{7: {Channel: channel}}, Rule: map[string]map[string][][]int{"default": {"gpt-5": {{7}}}}, ModelGroup: map[string]map[string]bool{"gpt-5": {"default": true}}}
	seedRequestResource(t, "file", "file_a", 1, 7)
	raw := []byte(`{"input":[{"type":"input_file","file_id":"file_a"}]}`)
	c := resourceRequestContext(raw, 1)
	c.Set("long_lived_principal_authenticated", true)
	c.Set("role", config.RoleAdminUser)
	if err := prepareResourceRequest(c, raw, "responses"); err != nil {
		t.Fatal(err)
	}
	return db, c
}

func TestResourceRequestOwnerRouteRevalidatesTokenAndGroup(t *testing.T) {
	_, c := setupResourceNewWorkTest(t)
	provider, _, err := GetProvider(c, "gpt-5")
	if err != nil || provider == nil {
		t.Fatalf("authorized owner inference rejected: %v", err)
	}
	settingValue, _ := c.Get("token_setting")
	setting := settingValue.(*model.TokenSetting)
	if setting.ResourceChannelID != 8 || c.GetInt("channel_id") != 7 {
		t.Fatalf("current SQL setting or owner priority lost: setting=%+v context=%+v", setting, c.Keys)
	}
	if c.GetInt("role") != config.RoleCommonUser {
		t.Fatal("stale administrator role survived SQL refresh")
	}
	if _, _, err := GetProvider(c, "not-allowed"); err == nil {
		t.Fatal("owner selection bypassed token model restriction")
	}
}

func TestResourceRequestOwnerPinDoesNotGrantAdminGroupBypass(t *testing.T) {
	_, c := setupResourceNewWorkTest(t)
	model.ChannelGroup.Lock()
	model.ChannelGroup.Rule = map[string]map[string][][]int{"private": {"gpt-5": {{7}}}}
	model.ChannelGroup.Unlock()
	if _, _, err := GetProvider(c, "gpt-5"); err == nil {
		t.Fatal("resource-derived pin bypassed current group authorization")
	}
}

func TestResourceRequestExplicitAdminPinRejectsRoleDowngrade(t *testing.T) {
	_, c := setupResourceNewWorkTest(t)
	c.Set("long_lived_admin_selected_channel", 7)
	if err := middleware.RefreshAuthenticatedLongLivedPrincipal(c); err == nil || err.StatusCode != http.StatusForbidden {
		t.Fatalf("downgraded admin pin accepted: %v", err)
	}
}

func TestResourceRequestDisabledOwnerChannelRejectsNewWork(t *testing.T) {
	db, c := setupResourceNewWorkTest(t)
	// SQL has current lifecycle authority even while another node's routing
	// snapshot still contains an enabled incarnation.
	if err := db.Model(&model.Channel{}).Where("id = ?", 7).Update("status", config.ChannelStatusManuallyDisabled).Error; err != nil {
		t.Fatal(err)
	}
	if _, _, err := GetProvider(c, "gpt-5"); err == nil {
		t.Fatal("disabled SQL owner channel admitted new provider work")
	}
}

// Ensure arbitrary future values in an otherwise valid envelope are not used
// as a reason to re-encode it during the authorization projection.
func TestResourceRequestRawNumericLexemesRemainIntact(t *testing.T) {
	setupRelayTestDB(t, &model.ResourceOwner{})
	raw := []byte(`{"input":"hello","future":123456789012345678901234567890.000e-8}`)
	before := bytes.Clone(raw)
	if err := prepareResourceRequest(resourceRequestContext(raw, 1), raw, "responses"); err != nil {
		t.Fatal(err)
	}
	if !json.Valid(raw) || !bytes.Equal(raw, before) {
		t.Fatalf("raw lexeme changed: %s", raw)
	}
}
