package middleware

import (
	"errors"
	"net/http"
	"testing"

	"one-api/common/authutil"
	"one-api/common/config"
	"one-api/common/groupctx"
	"one-api/common/logger"
	"one-api/model"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

func setupI007Principal(t *testing.T, role int, explicit bool) (*gin.Context, *gorm.DB) {
	t.Helper()
	useTestAuthDB(t)
	db := model.DB
	oldGroups := model.GlobalUserGroupRatio
	oldChannels, oldRules, oldMatch := model.ChannelGroup.Channels, model.ChannelGroup.Rule, model.ChannelGroup.Match
	oldRedis, oldLogger := config.RedisEnabled, logger.Logger
	config.RedisEnabled, logger.Logger = false, zap.NewNop()
	model.GlobalUserGroupRatio = &model.UserGroupRatio{}
	model.ChannelGroup.Channels = map[int]*model.ChannelChoice{
		1: {Channel: &model.Channel{Id: 1, Status: config.ChannelStatusEnabled}},
		2: {Channel: &model.Channel{Id: 2, Status: config.ChannelStatusEnabled}},
		3: {Channel: &model.Channel{Id: 3, Status: config.ChannelStatusEnabled}},
	}
	model.ChannelGroup.Rule = map[string]map[string][][]int{
		"default": {"primary-model": {{1}}},
		"backup":  {"backup-model": {{2}}},
		"outside": {"backup-model": {{3}}},
	}
	model.ChannelGroup.Match = nil
	t.Cleanup(func() {
		model.GlobalUserGroupRatio = oldGroups
		model.ChannelGroup.Channels, model.ChannelGroup.Rule, model.ChannelGroup.Match = oldChannels, oldRules, oldMatch
		config.RedisEnabled, logger.Logger = oldRedis, oldLogger
	})
	if err := db.AutoMigrate(&model.UserGroup{}, &model.PublicationVersion{}, &model.Channel{}); err != nil {
		t.Fatal(err)
	}
	for _, choice := range model.ChannelGroup.Channels {
		if err := db.Session(&gorm.Session{SkipHooks: true}).Create(choice.Channel).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := model.EnsurePublicationVersionRows(db); err != nil {
		t.Fatal(err)
	}
	for _, group := range []model.UserGroup{
		{Symbol: "default", Ratio: 1},
		{Symbol: "backup", Ratio: 2.5, Public: true},
		{Symbol: "outside", Ratio: 3},
	} {
		if err := group.Create(); err != nil {
			t.Fatal(err)
		}
	}
	token := createAuthTestToken(t, "i007-user", role)
	if err := db.Model(token).Update("backup_group", "backup").Error; err != nil {
		t.Fatal(err)
	}
	c, recorder := newAuthTestContext(http.MethodGet, "/v1/realtime")
	credential := authutil.Credential{Value: token.Key}
	if explicit {
		credential.SelectorParts = []string{"3"}
	}
	tokenAuth(c, credential)
	if c.IsAborted() {
		t.Fatalf("测试主体认证失败: %d %s", recorder.Code, recorder.Body.String())
	}
	if err := NewGroupDistributor(c).SetupGroups(); err != nil {
		t.Fatal(err)
	}
	return c, db
}

func TestFixI007_PreservesAuthorizedSelectedRoute(t *testing.T) {
	for _, route := range []string{"primary", "backup", "admin_explicit"} {
		t.Run(route, func(t *testing.T) {
			role := config.RoleCommonUser
			if route == "admin_explicit" {
				role = config.RoleAdminUser
			}
			c, _ := setupI007Principal(t, role, route == "admin_explicit")
			channelID, modelName, group, source, ratio := 1, "primary-model", "default", groupctx.RoutingGroupSourceTokenGroup, 1.0
			if route == "backup" {
				channelID, modelName, group, source, ratio = 2, "backup-model", "backup", groupctx.RoutingGroupSourceBackupGroup, 2.5
				groupctx.SetRoutingGroup(c, group, source)
				c.Set("is_backupGroup", true)
				c.Set("group_ratio", ratio)
			} else if route == "admin_explicit" {
				channelID, modelName = 3, "backup-model"
			}
			c.Set("channel_id", channelID)
			for turn := 0; turn < 2; turn++ {
				if err := RefreshAuthenticatedLongLivedPrincipal(c); err != nil {
					t.Fatalf("第 %d 轮刷新失败: %v", turn, err)
				}
				if err := EnsureLongLivedChannelAllowed(c, modelName); err != nil {
					t.Fatalf("第 %d 轮合法路由被拒绝: %v", turn, err)
				}
				if groupctx.CurrentRoutingGroup(c) != group || groupctx.CurrentRoutingGroupSource(c) != source || c.GetFloat64("group_ratio") != ratio || c.GetBool("is_backupGroup") != (route == "backup") {
					t.Fatalf("实际计费组丢失: %+v ratio=%v", groupctx.CurrentRoutingGroupMeta(c), c.GetFloat64("group_ratio"))
				}
			}
		})
	}
}

func TestFixI007_RejectsRevocationAndClearsAuthorization(t *testing.T) {
	for _, revoked := range []string{"token", "admin", "backup_declaration", "backup_public_access", "backup_disabled", "channel_membership"} {
		t.Run(revoked, func(t *testing.T) {
			role := config.RoleCommonUser
			if revoked == "admin" {
				role = config.RoleAdminUser
			}
			c, db := setupI007Principal(t, role, revoked == "admin")
			c.Set("channel_id", 2)
			groupctx.SetRoutingGroup(c, "backup", groupctx.RoutingGroupSourceBackupGroup)
			c.Set("is_backupGroup", true)
			c.Set("group_ratio", 2.5)
			if revoked == "admin" {
				c.Set("channel_id", 3)
			}
			var err error
			switch revoked {
			case "token":
				err = db.Model(&model.Token{}).Where("id = ?", c.GetInt("token_id")).Update("status", config.TokenStatusDisabled).Error
			case "admin":
				err = db.Model(&model.User{}).Where("id = ?", c.GetInt("id")).Update("role", config.RoleCommonUser).Error
			case "backup_declaration":
				err = db.Model(&model.Token{}).Where("id = ?", c.GetInt("token_id")).Update("backup_group", "").Error
			case "backup_public_access", "backup_disabled":
				var group model.UserGroup
				if err := db.Where("symbol = ?", "backup").First(&group).Error; err != nil {
					t.Fatal(err)
				}
				if revoked == "backup_disabled" {
					err = model.ChangeUserGroupEnable(group.Id, false)
				} else {
					group.Public = false
					err = group.Update()
				}
			case "channel_membership":
				delete(model.ChannelGroup.Rule["backup"], "backup-model")
			}
			if err != nil {
				t.Fatal(err)
			}
			principalErr := RefreshAuthenticatedLongLivedPrincipal(c)
			channelErr := EnsureLongLivedChannelAllowed(c, "backup-model")
			if principalErr == nil && channelErr == nil {
				t.Fatal("撤权后仍允许新工作")
			}
			if channelErr == nil || c.GetBool("is_backupGroup") || c.GetFloat64("group_ratio") != 0 {
				t.Fatalf("撤权后保留了有效准入: channelErr=%v backup=%v ratio=%v", channelErr, c.GetBool("is_backupGroup"), c.GetFloat64("group_ratio"))
			}
		})
	}
}

func TestFixI007_OwnerPinDoesNotGrantAdminSelection(t *testing.T) {
	for _, role := range []int{config.RoleCommonUser, config.RoleAdminUser} {
		c, _ := setupI007Principal(t, role, false)
		c.Set("specific_channel_id", 3)
		c.Set("channel_id", 3)
		if err := RefreshAuthenticatedLongLivedPrincipal(c); err != nil {
			t.Fatal(err)
		}
		if err := EnsureLongLivedChannelAllowed(c, "backup-model"); err == nil {
			t.Fatalf("仅 owner/channel pin 不应取得管理员显式选路权限，role=%d", role)
		}
	}
}

func TestFixI007_IgnoredAdminSelectorDoesNotAuthorizeOwnerPin(t *testing.T) {
	initial, _ := setupI007Principal(t, config.RoleAdminUser, false)
	token, err := model.GetTokenByIdWithContext(initial.Request.Context(), initial.GetInt("token_id"))
	if err != nil {
		t.Fatal(err)
	}
	c, _ := newAuthTestContext(http.MethodGet, "/v1/responses")
	tokenAuth(c, authutil.Credential{Value: token.Key, SelectorParts: []string{"3", "ignore"}})
	if c.IsAborted() || !c.GetBool("specific_channel_id_ignore") {
		t.Fatal("测试未建立被忽略的管理员 selector")
	}
	if err := NewGroupDistributor(c).SetupGroups(); err != nil {
		t.Fatal(err)
	}
	// 模拟资源 owner 固定执行身份；不能把先前被忽略的 selector 变为授权来源。
	c.Set("specific_channel_id", 3)
	c.Set("specific_channel_id_ignore", false)
	c.Set("channel_id", 3)
	if err := RefreshAuthenticatedLongLivedPrincipal(c); err != nil {
		t.Fatal(err)
	}
	if err := EnsureLongLivedChannelAllowed(c, "backup-model"); err == nil {
		t.Fatal("被忽略的 selector 使 owner pin 越过当前组授权")
	}
}

func TestFixI007_BackupSelectionSurvivesTemporaryPrincipalReadFailure(t *testing.T) {
	for _, table := range []string{"tokens", "publication_versions"} {
		t.Run(table, func(t *testing.T) {
			c, db := setupI007Principal(t, config.RoleCommonUser, false)
			c.Set("channel_id", 2)
			groupctx.SetRoutingGroup(c, "backup", groupctx.RoutingGroupSourceBackupGroup)
			if err := RefreshAuthenticatedLongLivedPrincipal(c); err != nil {
				t.Fatal(err)
			}
			if err := EnsureLongLivedChannelAllowed(c, "backup-model"); err != nil {
				t.Fatal(err)
			}
			const callback = "test:i007:temporary_principal_failure"
			if err := db.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
				if tx.Statement.Table == table {
					tx.AddError(errors.New("I-007 临时读取故障"))
				}
			}); err != nil {
				t.Fatal(err)
			}
			principalErr := RefreshAuthenticatedLongLivedPrincipal(c)
			channelErr := EnsureLongLivedChannelAllowed(c, "backup-model")
			if err := db.Callback().Query().Remove(callback); err != nil {
				t.Fatal(err)
			}
			if principalErr == nil || channelErr == nil || c.GetBool("is_backupGroup") || c.GetFloat64("group_ratio") != 0 {
				t.Fatal("读取故障未清除当前有效准入")
			}
			if err := RefreshAuthenticatedLongLivedPrincipal(c); err != nil {
				t.Fatalf("数据库恢复后仍不能刷新: %v", err)
			}
			if err := EnsureLongLivedChannelAllowed(c, "backup-model"); err != nil {
				t.Fatalf("数据库恢复后合法备用组被遗忘: %v", err)
			}
			if groupctx.CurrentRoutingGroup(c) != "backup" || !c.GetBool("is_backupGroup") || c.GetFloat64("group_ratio") != 2.5 {
				t.Fatal("恢复后计费组不再是实际备用组")
			}
			if err := db.Model(&model.Token{}).Where("id = ?", c.GetInt("token_id")).Update("backup_group", "").Error; err != nil {
				t.Fatal(err)
			}
			if err := RefreshAuthenticatedLongLivedPrincipal(c); err == nil {
				t.Fatal("真正撤销备用组声明后仍允许恢复")
			}
			if err := EnsureLongLivedChannelAllowed(c, "backup-model"); err == nil {
				t.Fatal("真正撤权后保留了有效渠道准入")
			}
		})
	}
}
