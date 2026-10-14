package base

import (
	"bytes"
	"encoding/json"

	"one-api/common/jsonobject"
)

// ApplyRawCustomParams 复用既有参数规则，只覆盖发生变化的顶层 raw 字段。
// pre 区分转换前后两个阶段，pre_add 配置在转换前执行且只执行一次。
func ApplyRawCustomParams(raw []byte, params map[string]interface{}, modelName string, pre bool) ([]byte, error) {
	preAdd, _ := params["pre_add"].(bool)
	if len(params) == 0 || pre != preAdd {
		return raw, nil
	}
	object, err := jsonobject.Parse(raw)
	if err != nil {
		return nil, err
	}
	var body map[string]interface{}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&body); err != nil {
		return nil, err
	}
	before, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	body = ApplyCustomParams(body, params, modelName, true)
	after, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	if bytes.Equal(before, after) {
		return raw, nil
	}
	beforeObject, err := jsonobject.Parse(before)
	if err != nil {
		return nil, err
	}
	afterObject, err := jsonobject.Parse(after)
	if err != nil {
		return nil, err
	}
	for name := range object.Fields {
		if _, exists := afterObject.Fields[name]; !exists {
			object.Delete(name)
		}
	}
	for name, value := range afterObject.Fields {
		if !bytes.Equal(beforeObject.Fields[name], value) {
			if err := object.SetRaw(name, value); err != nil {
				return nil, err
			}
		}
	}
	return object.MarshalJSON()
}
