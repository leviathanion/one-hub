package middleware

import (
	"testing"

	"one-api/common/config"
	"one-api/common/groupctx"
	"one-api/model"
)

func TestFixI043_AuthorizesOwnerThroughCurrentDeclaredBackup(t *testing.T) {
	c, _ := setupI007Principal(t, config.RoleCommonUser, false)
	if err := RefreshAuthenticatedLongLivedPrincipal(c); err != nil {
		t.Fatal(err)
	}
	c.Set("channel_id", 2)
	if err := EnsureLongLivedChannelAllowed(c, "backup-model"); err != nil {
		t.Fatalf("有当前备用组授权的 owner 被拒绝: %v", err)
	}
	if groupctx.CurrentRoutingGroup(c) != "backup" || groupctx.CurrentRoutingGroupSource(c) != groupctx.RoutingGroupSourceBackupGroup || !c.GetBool("is_backupGroup") || c.GetFloat64("group_ratio") != 2.5 {
		t.Fatalf("owner 未使用实际备用组计费: %+v ratio=%v", groupctx.CurrentRoutingGroupMeta(c), c.GetFloat64("group_ratio"))
	}
}

func TestFixI043_SelectionAfterRefreshCannotBypassGroupAuthorization(t *testing.T) {
	for _, revoked := range []string{"public_access", "backup_declaration"} {
		t.Run(revoked, func(t *testing.T) {
			c, db := setupI007Principal(t, config.RoleCommonUser, false)
			if revoked == "public_access" {
				var group model.UserGroup
				if err := db.Where("symbol = ?", "backup").First(&group).Error; err != nil {
					t.Fatal(err)
				}
				group.Public = false
				if err := group.Update(); err != nil {
					t.Fatal(err)
				}
			} else if err := db.Model(&model.Token{}).Where("id = ?", c.GetInt("token_id")).Update("backup_group", "").Error; err != nil {
				t.Fatal(err)
			}
			if err := RefreshAuthenticatedLongLivedPrincipal(c); err != nil {
				t.Fatalf("正常主组主体无法刷新: %v", err)
			}
			// 首轮刷新后才选中的旧备用渠道，必须重新验证实际选组来源。
			c.Set("channel_id", 2)
			groupctx.SetRoutingGroup(c, "backup", groupctx.RoutingGroupSourceBackupGroup)
			c.Set("is_backupGroup", true)
			c.Set("group_ratio", 2.5)
			if err := EnsureLongLivedChannelAllowed(c, "backup-model"); err == nil {
				t.Fatal("后选备用渠道越过当前组权限")
			}
			if c.GetBool("is_backupGroup") || c.GetFloat64("group_ratio") != 0 {
				t.Fatal("拒绝后残留有效计费组")
			}
		})
	}
}
