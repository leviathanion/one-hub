package model

import (
	"context"
	"errors"
)

type UserRoutingState struct {
	Id     int
	Group  string
	Status int
	Role   int
}

// GetUserRoutingState 从权威写库读取一个工作单元的用户归属，不经过缓存。
func GetUserRoutingState(ctx context.Context, userID int) (*UserRoutingState, error) {
	if DB == nil || userID <= 0 {
		return nil, errors.New("用户归属存储不可用")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var user User
	if err := DB.WithContext(ctx).Select("id", "group", "status", "role").First(&user, "id = ?", userID).Error; err != nil {
		return nil, err
	}
	return &UserRoutingState{Id: user.Id, Group: user.Group, Status: user.Status, Role: user.Role}, nil
}

// GetAuthorizedUserGroup 使用调用方已刷新的一次主体与分组策略判断选组权限。
func GetAuthorizedUserGroup(user *UserRoutingState, symbol string) (*UserGroup, error) {
	if user == nil {
		return nil, errors.New("用户归属不可用")
	}
	group := GlobalUserGroupRatio.GetBySymbol(symbol)
	if group == nil {
		return nil, errors.New("无效的用户组")
	}
	if !group.Public && user.Group != symbol {
		return nil, errors.New("用户无权使用指定的分组")
	}
	return group, nil
}
