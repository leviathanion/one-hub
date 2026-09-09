package middleware

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"one-api/common"
	"one-api/common/config"
	"one-api/common/groupctx"
	"one-api/model"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

const longLivedPrincipalAuthenticatedKey = "long_lived_principal_authenticated"
const longLivedPrincipalCurrentKey = "long_lived_principal_current"
const longLivedAdminSelectedChannelKey = "long_lived_admin_selected_channel"

func markLongLivedPrincipalAuthenticated(c *gin.Context) {
	if c != nil {
		c.Set(longLivedPrincipalAuthenticatedKey, true)
	}
}

// RefreshAuthenticatedLongLivedPrincipal applies revalidation only to a
// context that passed tokenAuth. This keeps internal/unit contexts that do not
// represent an authenticated wire request outside the security contract.
func RefreshAuthenticatedLongLivedPrincipal(c *gin.Context) *types.OpenAIErrorWithStatusCode {
	if c == nil || !c.GetBool(longLivedPrincipalAuthenticatedKey) {
		return nil
	}
	return RefreshLongLivedPrincipal(c)
}

func IsAuthenticatedLongLivedPrincipal(c *gin.Context) bool {
	return c != nil && c.GetBool(longLivedPrincipalAuthenticatedKey)
}

// AdmitAuthenticatedChannelWork 在实际渠道确定后、预扣或供应商工作前重新确认当前授权。
func AdmitAuthenticatedChannelWork(c *gin.Context, modelName string, channelID int) *types.OpenAIErrorWithStatusCode {
	if c == nil {
		return common.StringErrorWrapperLocal("request context is required", "invalid_request", http.StatusBadRequest)
	}
	if !IsAuthenticatedLongLivedPrincipal(c) {
		return nil
	}
	c.Set("channel_id", channelID)
	if apiErr := RefreshLongLivedPrincipal(c); apiErr != nil {
		return apiErr
	}
	if err := EnsureTokenModelAllowed(c, modelName); err != nil {
		clearLongLivedRouteAuthorization(c)
		return common.ErrorWrapperLocal(err, "permission_denied", http.StatusForbidden)
	}
	if err := EnsureLongLivedChannelAllowed(c, modelName); err != nil {
		return common.ErrorWrapperLocal(err, "permission_denied", http.StatusForbidden)
	}
	return nil
}

// RefreshLongLivedPrincipal 从 SQL 重新确认一次新工作的主体和选组权限。
// HTTP input_tokens 同样无条件执行此检查；此处不读取余额或进行预扣。
func RefreshLongLivedPrincipal(c *gin.Context) (apiErr *types.OpenAIErrorWithStatusCode) {
	if c == nil {
		return common.StringErrorWrapperLocal("request context is required", "invalid_request", http.StatusBadRequest)
	}
	selectedGroup, selectedSource := groupctx.CurrentRoutingGroup(c), groupctx.CurrentRoutingGroupSource(c)
	c.Set(longLivedPrincipalCurrentKey, false)
	defer func() {
		if apiErr != nil {
			clearLongLivedRouteAuthorization(c)
		}
	}()
	readCtx, cancel := principalReadContext(c)
	defer cancel()
	token, user, principalErr := loadCurrentPrincipal(c, readCtx)
	if principalErr != nil {
		return principalErr
	}
	if c.GetInt(longLivedAdminSelectedChannelKey) > 0 && !c.GetBool("specific_channel_id_ignore") && user.Role < config.RoleAdminUser {
		return common.StringErrorWrapperLocal("explicit channel selection requires current administrator permission", "permission_denied", http.StatusForbidden)
	}
	if err := model.EnsureUserGroupPolicyAvailable(readCtx); err != nil {
		return common.ErrorWrapperLocal(err, "principal_group_read_failed", http.StatusServiceUnavailable)
	}

	c.Set("group", user.Group)
	c.Set("role", user.Role)
	c.Set("token_group", token.Group)
	c.Set("token_backup_group", token.BackupGroup)
	c.Set("token_unlimited_quota", token.UnlimitedQuota)

	distributor := NewGroupDistributor(c)
	effectiveGroup, source := distributor.determineEffectiveGroup(token.Group, token.BackupGroup, user.Group)
	// 已经使用备用组的连接仍须以该组重新准入；SQL 刷新不构成重新选路。
	if selectedSource == groupctx.RoutingGroupSourceBackupGroup {
		if selectedGroup == "" || selectedGroup != strings.TrimSpace(token.BackupGroup) {
			return common.StringErrorWrapperLocal("selected backup group is no longer authorized", "permission_denied", http.StatusForbidden)
		}
		effectiveGroup, source = selectedGroup, groupctx.RoutingGroupSourceBackupGroup
	}
	group, err := model.GetAuthorizedUserGroup(user, effectiveGroup)
	if err != nil {
		return common.ErrorWrapperLocal(err, "permission_denied", http.StatusForbidden)
	}
	setAuthorizedRoutingGroup(c, effectiveGroup, source, group.Ratio)
	c.Set(longLivedPrincipalCurrentKey, true)
	return nil
}

func EnsureLongLivedChannelAllowed(c *gin.Context, modelName string) (err error) {
	if c == nil {
		return errors.New("request context is required")
	}
	if !IsAuthenticatedLongLivedPrincipal(c) {
		return nil
	}
	defer func() {
		if err != nil {
			clearLongLivedRouteAuthorization(c)
		}
	}()
	if !c.GetBool(longLivedPrincipalCurrentKey) {
		return errors.New("current principal authorization is required")
	}
	channelID := c.GetInt("channel_id")
	if channelID <= 0 {
		return errors.New("selected channel is required")
	}
	group := groupctx.CurrentRoutingGroup(c)
	source := groupctx.CurrentRoutingGroupSource(c)
	if source == groupctx.RoutingGroupSourceBackupGroup {
		if group == "" || group != strings.TrimSpace(c.GetString("token_backup_group")) {
			return errors.New("selected backup group is no longer authorized")
		}
	} else {
		primary, primarySource := NewGroupDistributor(c).determineEffectiveGroup(c.GetString("token_group"), c.GetString("token_backup_group"), c.GetString("group"))
		if group != primary || source != primarySource {
			return errors.New("selected group no longer matches the current principal")
		}
	}
	user := &model.UserRoutingState{Id: c.GetInt("id"), Group: c.GetString("group"), Role: c.GetInt("role")}
	groupPolicy, err := model.GetAuthorizedUserGroup(user, group)
	if err != nil {
		return err
	}
	setAuthorizedRoutingGroup(c, group, source, groupPolicy.Ratio)
	// 只有 tokenAuth 解析出的显式选择才可使用管理员权限；资源 owner pin 没有这个来源。
	if c.GetInt(longLivedAdminSelectedChannelKey) == channelID && c.GetInt("specific_channel_id") == channelID && !c.GetBool("specific_channel_id_ignore") && c.GetInt("role") >= config.RoleAdminUser {
		readCtx, cancel := principalReadContext(c)
		defer cancel()
		channel, err := model.GetChannelByIdWithContext(readCtx, channelID)
		if err != nil {
			return err
		}
		if channel.Status != config.ChannelStatusEnabled {
			return errors.New("selected channel is disabled")
		}
		return nil
	}
	eligible, err := model.ChannelGroup.PreferredChannelEligible(group, strings.TrimSpace(modelName), channelID)
	if err == nil && eligible {
		return nil
	}
	// owner 固定渠道不经过普通选路；仅尝试当前声明且获授权的备用组，不切换渠道。
	backup := strings.TrimSpace(c.GetString("token_backup_group"))
	if source != groupctx.RoutingGroupSourceBackupGroup && backup != "" && backup != group {
		backupPolicy, authErr := model.GetAuthorizedUserGroup(user, backup)
		if authErr != nil {
			return authErr
		}
		if eligible, eligibilityErr := model.ChannelGroup.PreferredChannelEligible(backup, strings.TrimSpace(modelName), channelID); eligibilityErr == nil && eligible {
			setAuthorizedRoutingGroup(c, backup, groupctx.RoutingGroupSourceBackupGroup, backupPolicy.Ratio)
			return nil
		}
	}
	return errors.New("selected channel is not allowed for current principal group")
}

func setAuthorizedRoutingGroup(c *gin.Context, group, source string, ratio float64) {
	groupctx.SetRoutingGroup(c, group, source)
	c.Set("group_ratio", ratio)
	c.Set("is_backupGroup", source == groupctx.RoutingGroupSourceBackupGroup)
}

func clearLongLivedRouteAuthorization(c *gin.Context) {
	c.Set(longLivedPrincipalCurrentKey, false)
	c.Set("role", 0)
	c.Set("is_backupGroup", false)
	c.Set("group_ratio", float64(0))
	// 已选路由来源是连接事实，保留供恢复后的 SQL 重验；它本身不授予新工作权限。
}

func EnsureTokenModelAllowed(c *gin.Context, modelName string) error {
	if c == nil {
		return errors.New("request context is required")
	}
	value, exists := c.Get("token_setting")
	if !exists {
		return nil
	}
	setting, ok := value.(*model.TokenSetting)
	if !ok || setting == nil || !setting.Limits.LimitModelSetting.Enabled {
		return nil
	}
	allowed := setting.Limits.LimitModelSetting.Models
	if len(allowed) == 0 {
		return errors.New("No available models configured for current token")
	}
	modelName = strings.TrimSpace(modelName)
	for _, candidate := range allowed {
		if strings.TrimSpace(candidate) == modelName {
			return nil
		}
	}
	return fmt.Errorf("Model %s is not supported for current token", modelName)
}
