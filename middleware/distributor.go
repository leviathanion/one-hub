package middleware

import (
	"fmt"
	"net/http"
	"one-api/common/groupctx"
	"one-api/model"
	"strings"

	"github.com/gin-gonic/gin"
)

// GroupDistributor 统一分组分发逻辑
type GroupDistributor struct {
	context *gin.Context
}

// NewGroupDistributor 创建分组分发器
func NewGroupDistributor(c *gin.Context) *GroupDistributor {
	return &GroupDistributor{context: c}
}

// SetupGroups 设置用户分组和令牌分组
func (gd *GroupDistributor) SetupGroups() error {
	userId := gd.context.GetInt("id")
	userGroup, _ := model.CacheGetUserGroup(userId)
	gd.context.Set("group", userGroup)

	tokenGroup := gd.context.GetString("token_group")
	backupGroup := gd.context.GetString("token_backup_group")

	// 建立请求级别的实际路由分组，不回写 token 的声明分组。
	effectiveGroup, groupSource := gd.determineEffectiveGroup(tokenGroup, backupGroup, userGroup)
	groupctx.SetRoutingGroup(gd.context, effectiveGroup, groupSource)
	gd.context.Set("is_backupGroup", false)

	// 设置分组比例
	return gd.setGroupRatio(effectiveGroup)
}

// determineEffectiveGroup 确定请求初始化阶段的有效分组。
func (gd *GroupDistributor) determineEffectiveGroup(tokenGroup, backupGroup, userGroup string) (string, string) {
	tokenGroup = strings.TrimSpace(tokenGroup)
	backupGroup = strings.TrimSpace(backupGroup)
	userGroup = strings.TrimSpace(userGroup)

	if tokenGroup != "" {
		return tokenGroup, groupctx.RoutingGroupSourceTokenGroup
	}
	if userGroup != "" {
		return userGroup, groupctx.RoutingGroupSourceUserGroup
	}
	if backupGroup != "" {
		return backupGroup, groupctx.RoutingGroupSourceBackupGroup
	}
	return "", ""
}

// setGroupRatio 设置分组倍率
func (gd *GroupDistributor) setGroupRatio(group string) error {
	groupRatio := model.GlobalUserGroupRatio.GetBySymbol(group)
	if groupRatio == nil {
		abortWithMessage(gd.context, http.StatusForbidden, fmt.Sprintf("分组 %s 不存在", group))
		return fmt.Errorf("分组 %s 不存在", group)
	}

	gd.context.Set("group_ratio", groupRatio.Ratio)
	return nil
}

func Distribute() func(c *gin.Context) {
	return func(c *gin.Context) {
		distributor := NewGroupDistributor(c)
		if err := distributor.SetupGroups(); err != nil {
			return
		}
		c.Next()
	}
}
