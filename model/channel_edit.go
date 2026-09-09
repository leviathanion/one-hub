package model

import (
	"encoding/json"
	"reflect"
	"strings"
)

// ChannelEditRequest 记录实际提交的字段，普通编辑可明确写入零值和清空值。
// Channel 只用于解码；数据库更新列始终由 channelMetadataFields 决定。
type ChannelEditRequest struct {
	Channel
	SubmittedFields map[string]json.RawMessage `json:"-"`
}

func (request *ChannelEditRequest) UnmarshalJSON(data []byte) error {
	if err := json.Unmarshal(data, &request.Channel); err != nil {
		return err
	}
	return json.Unmarshal(data, &request.SubmittedFields)
}

func (request *ChannelEditRequest) Update() error {
	return request.Channel.UpdateWithOptions(false, ChannelUpdateOptions{
		SubmittedFields:     request.SubmittedFields,
		AllowIdentityChange: true,
	})
}

// 元数据编辑的唯一列白名单。身份列经管理编辑授权后写入，运行统计不接受表单覆盖。
var channelMetadataFields = []string{
	"Status", "Name", "Weight", "Other", "Models", "Group", "Tag",
	"ModelMapping", "ModelHeaders", "CustomParameter", "Priority", "Proxy",
	"TestModel", "OnlyChat", "PreCost", "CompatibleResponse", "AllowExtraBody",
	"DisabledStream", "Plugin",
}

func (options ChannelUpdateOptions) submitted(name string) bool {
	_, ok := options.SubmittedFields[name]
	return ok
}

func channelMetadataUpdate(persisted, input *Channel, overwrite bool, options ChannelUpdateOptions) (*Channel, map[string]any) {
	candidate := *persisted
	destination := reflect.ValueOf(&candidate).Elem()
	source := reflect.ValueOf(input).Elem()
	updates := make(map[string]any)
	for _, name := range channelMetadataFields {
		field, _ := source.Type().FieldByName(name)
		column := strings.Split(field.Tag.Get("json"), ",")[0]
		value := source.FieldByName(name)
		submitted := overwrite || !value.IsZero()
		if options.SubmittedFields != nil {
			submitted = options.submitted(column)
		} else if name == "Other" {
			submitted = options.OtherSubmitted || strings.TrimSpace(input.Other) != ""
		}
		if !submitted {
			continue
		}
		destination.FieldByName(name).Set(value)
		if value.Kind() == reflect.Ptr && value.IsNil() {
			updates[column] = nil
		} else {
			updates[column] = value.Interface()
		}
	}
	// 身份输入先用于比较，是否写入由管理编辑授权决定；不补回未提交的 Key。
	if input.Type != 0 || options.submitted("type") {
		candidate.Type = input.Type
	}
	if input.Key != "" || options.submitted("key") {
		candidate.Key = input.Key
	}
	if input.BaseURL != nil || options.BaseURLSubmitted || options.submitted("base_url") {
		candidate.BaseURL = input.BaseURL
	}
	return &candidate, updates
}
