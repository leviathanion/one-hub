package relay

import (
	"encoding/json"
	"one-api/common/jsonobject"
	"one-api/model"
	providersBase "one-api/providers/base"
)

// effectiveNativeRequestBody 同时用于候选评估与选中后的准备。
func effectiveNativeRequestBody(channel *model.Channel, modelName string, raw []byte, ownsModel bool) ([]byte, map[string]json.RawMessage, error) {
	if channel != nil {
		params, err := channel.GetCustomParameterMap()
		if err != nil {
			return nil, nil, err
		}
		raw, err = providersBase.ApplyRawCustomParams(raw, params, modelName, true)
		if err != nil {
			return nil, nil, err
		}
	}
	object, err := jsonobject.Parse(raw)
	if err != nil {
		return nil, nil, err
	}
	if ownsModel {
		if err := object.SetJSON("model", modelName); err != nil {
			return nil, nil, err
		}
	}
	body, err := object.MarshalJSON()
	return body, object.Fields, err
}
