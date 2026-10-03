// Package credentials owns persisted credential operations independently of any
// upstream protocol. Providers classify dispatch/results; this package never
// sends OAuth requests or interprets token payloads.
package credentials

import (
	"encoding/json"
	"errors"
	"strings"
)

type Refresh struct {
	AttemptID string `json:"attempt_id"`
	StartedAt int64  `json:"started_at"`
}

func object(raw []byte) (map[string]json.RawMessage, error) {
	if len(raw) == 0 {
		return map[string]json.RawMessage{}, nil
	}
	var value map[string]json.RawMessage
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		return nil, errors.New("invalid channel business data")
	}
	return value, nil
}

func ReadRefresh(raw []byte) (*Refresh, error) {
	root, err := object(raw)
	if err != nil {
		return nil, err
	}
	section, err := object(root["credentials"])
	if err != nil {
		return nil, err
	}
	value, ok := section["refresh"]
	if !ok {
		return nil, nil
	}
	var refresh Refresh
	if err := json.Unmarshal(value, &refresh); err != nil || strings.TrimSpace(refresh.AttemptID) == "" {
		return nil, errors.New("invalid credential refresh state")
	}
	return &refresh, nil
}

// WriteRefresh preserves every namespace and unrelated credential field.
func WriteRefresh(raw []byte, refresh *Refresh) ([]byte, error) {
	root, err := object(raw)
	if err != nil {
		return nil, err
	}
	section, err := object(root["credentials"])
	if err != nil {
		return nil, err
	}
	if refresh == nil {
		delete(section, "refresh")
	} else {
		if strings.TrimSpace(refresh.AttemptID) == "" {
			return nil, errors.New("empty refresh attempt")
		}
		section["refresh"], err = json.Marshal(refresh)
		if err != nil {
			return nil, err
		}
	}
	if len(section) == 0 {
		delete(root, "credentials")
	} else {
		root["credentials"], err = json.Marshal(section)
		if err != nil {
			return nil, err
		}
	}
	return json.Marshal(root)
}

func RequireEditable(raw []byte) error {
	refresh, err := ReadRefresh(raw)
	if err != nil {
		return err
	}
	if refresh != nil {
		return errors.New("渠道凭据正在刷新或结果尚未确定，请完成凭据恢复后再修改连接配置")
	}
	return nil
}
