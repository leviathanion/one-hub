package groupctx

import (
	"one-api/common/config"
	"strings"

	"github.com/gin-gonic/gin"
)

const (
	RoutingGroupSourceTokenGroup  = "token_group"
	RoutingGroupSourceUserGroup   = "user_group"
	RoutingGroupSourceBackupGroup = "backup_group"
)

func DeclaredTokenGroup(c *gin.Context) string {
	return contextString(c, "token_group")
}

func BackupGroup(c *gin.Context) string {
	return contextString(c, "token_backup_group")
}

func UserGroup(c *gin.Context) string {
	return contextString(c, "group")
}

func CurrentRoutingGroup(c *gin.Context) string {
	if group := contextString(c, config.GinRoutingGroupKey); group != "" {
		return group
	}
	if group := DeclaredTokenGroup(c); group != "" {
		return group
	}
	if group := UserGroup(c); group != "" {
		return group
	}
	return BackupGroup(c)
}

func CurrentRoutingGroupSource(c *gin.Context) string {
	if source := contextString(c, config.GinRoutingGroupSourceKey); source != "" {
		return source
	}

	current := CurrentRoutingGroup(c)
	switch {
	case current == "":
		return ""
	case current == DeclaredTokenGroup(c):
		return RoutingGroupSourceTokenGroup
	case current == UserGroup(c):
		return RoutingGroupSourceUserGroup
	case current == BackupGroup(c):
		return RoutingGroupSourceBackupGroup
	default:
		return ""
	}
}

func SetRoutingGroup(c *gin.Context, group, source string) {
	if c == nil {
		return
	}

	c.Set(config.GinRoutingGroupKey, strings.TrimSpace(group))
	c.Set(config.GinRoutingGroupSourceKey, strings.TrimSpace(source))
}

func CurrentRoutingGroupMeta(c *gin.Context) map[string]any {
	if c == nil {
		return nil
	}

	meta := map[string]any{
		"is_backup_group": c.GetBool("is_backupGroup"),
	}
	if usingGroup := CurrentRoutingGroup(c); usingGroup != "" {
		meta["using_group"] = usingGroup
	}
	if tokenGroup := DeclaredTokenGroup(c); tokenGroup != "" {
		meta["token_group"] = tokenGroup
	}
	if backupGroup := BackupGroup(c); backupGroup != "" {
		meta["backup_group_name"] = backupGroup
	}
	if source := CurrentRoutingGroupSource(c); source != "" {
		meta["routing_group_source"] = source
	}
	return meta
}

func contextString(c *gin.Context, key string) string {
	if c == nil {
		return ""
	}
	return strings.TrimSpace(c.GetString(key))
}
