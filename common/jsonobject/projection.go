package jsonobject

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"reflect"
	"strings"
)

// Projection 保存已知字段的原始值和解码后的快照。重新编码时，只有
// typed 值实际变化的字段覆盖原值；未知字段由调用方的 ExtraFields 持有。
// 它不参与跨协议映射，也不为未知字段声明协议等价性。
type Projection struct {
	raw  map[string]json.RawMessage
	hash map[string][sha256.Size]byte
}

// DecodeProjection 的 target 必须是没有 JSON 方法的 wire struct 别名指针。
// UseNumber 使开放的 any 子树在局部修改后仍能保留大整数及指数表示。
func DecodeProjection(data []byte, target any) (*Projection, map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return nil, nil, err
	}
	return CaptureProjection(data, target)
}

// CaptureProjection 用于已有自定义解码或字段规范化的 wire struct。
func CaptureProjection(data []byte, value any) (*Projection, map[string]json.RawMessage, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, nil, err
	}
	known, err := projectionFields(value)
	if err != nil {
		return nil, nil, err
	}
	names := projectionFieldNames(value)
	state := &Projection{raw: make(map[string]json.RawMessage), hash: make(map[string][sha256.Size]byte, len(known))}
	var extra map[string]json.RawMessage
	for key, field := range raw {
		if _, ok := names[key]; ok {
			state.raw[key] = field
		} else {
			if extra == nil {
				extra = make(map[string]json.RawMessage)
			}
			extra[key] = field
		}
	}
	for key, field := range known {
		state.hash[key] = sha256.Sum256(field)
	}
	return state, extra, nil
}

// MarshalProjection 不修改 state 或 extra，值拷贝后的 DTO 可以独立重编码。
// ExtraFields 不能覆盖已知字段；这些字段始终由 typed 值及其原始快照拥有。
func MarshalProjection(value any, extra map[string]json.RawMessage, state *Projection) ([]byte, error) {
	known, err := projectionFields(value)
	if err != nil {
		return nil, err
	}
	names := projectionFieldNames(value)
	fields := make(map[string]json.RawMessage, len(extra)+len(known))
	for key, raw := range extra {
		if _, owned := names[key]; !owned {
			fields[key] = raw
		}
	}
	for name := range names {
		current, present := known[name]
		if state != nil {
			previousHash, wasPresent := state.hash[name]
			if present == wasPresent && (!present || sha256.Sum256(current) == previousHash) {
				if original, exists := state.raw[name]; exists {
					fields[name] = original
				}
				continue
			}
		}
		if present {
			fields[name] = current
		}
	}
	return json.Marshal(fields)
}

func projectionFields(value any) (map[string]json.RawMessage, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return nil, err
	}
	return fields, nil
}

// 这里只枚举 wire 别名直接声明的字段，不改变字段值或建立通用反射转换器。
func projectionFieldNames(value any) map[string]struct{} {
	t := reflect.TypeOf(value)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	names := make(map[string]struct{}, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if field.PkgPath != "" {
			continue
		}
		name := strings.SplitN(field.Tag.Get("json"), ",", 2)[0]
		if name == "-" {
			continue
		}
		if name == "" {
			name = field.Name
		}
		names[name] = struct{}{}
	}
	return names
}
