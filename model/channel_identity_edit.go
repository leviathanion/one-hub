package model

import (
	"errors"
	"strings"
)

func prepareChannelIdentityEdit(persisted, candidate *Channel, allowIdentityChange bool) (bool, error) {
	// 历史 Other 字符串按既有规则解释；规范化本身不是账号变更，也不回写未提交字段。
	original := *persisted
	err := original.CanonicalizeRuntimeConfigJSON()
	if err == nil {
		err = validateChannelIncarnationMutation(&original, candidate)
	}
	if err == nil {
		return false, nil
	}
	// 调用方已校验新配置；无法解释旧配置时，管理员修复也按身份变更处理。
	if !allowIdentityChange {
		return false, err
	}
	if candidate.Key != persisted.Key && strings.TrimSpace(candidate.Key) == "" {
		return false, errors.New("渠道凭据不能为空；保留原凭据时请不要提交 key")
	}
	if persisted.CredentialRefreshFence != nil {
		return false, errors.New("渠道凭据正在刷新或结果尚未确定，请完成凭据恢复后再修改连接配置")
	}
	return true, nil
}
