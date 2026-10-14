package base

import (
	"bytes"
	"encoding/json"

	"one-api/common/providerresponse"
	commonresponses "one-api/common/responses"
	"one-api/model"
)

// 这些字段在 provider 构造请求前已决定生命周期准入、资源归属及选路，
// 渠道参数只能调整上游参数，不能改写已经确定的生命周期事实。
var responsesLifecycleFieldsOwnedByRelay = []string{
	"background",
	"conversation",
	"previous_response_id",
	"prompt",
	"store",
}

type responsesLifecycleFieldState struct {
	present bool
	value   json.RawMessage
}

// ApplyResponsesCustomParameters 复用参数合并规则，并报告生命周期事实是否变化。
func ApplyResponsesCustomParameters(bodyMap map[string]interface{}, customParams map[string]interface{}, modelName string, inputTokens, applyPreAdd bool) (map[string]interface{}, string, bool, error) {
	lifecycleFields := responsesLifecycleFieldsOwnedByRelay
	if inputTokens {
		lifecycleFields = []string{"previous_response_id"}
	}
	lifecycleBefore, err := snapshotResponsesLifecycleFields(bodyMap, lifecycleFields)
	if err != nil {
		return nil, "", false, err
	}
	bodyMap = ApplyCustomParams(bodyMap, customParams, modelName, applyPreAdd)
	field, changed, err := changedResponsesLifecycleField(lifecycleBefore, bodyMap, lifecycleFields)
	return bodyMap, field, changed, err
}

// ValidateResponsesCustomParameterCompatibility 在临时投影上执行与请求构造相同的
// 生命周期保护，让选路在 provider work 前排除不能保持请求语义的候选。
func ValidateResponsesCustomParameterCompatibility(channel *model.Channel, fields map[string]json.RawMessage, modelName string, operation providerresponse.Operation, applyPreAdd bool) error {
	if channel == nil {
		return nil
	}
	customParams, err := channel.GetCustomParameterMap()
	if err != nil {
		return &RequestCapabilityError{Param: "custom_parameter", Message: "channel custom parameters are invalid"}
	}
	if len(customParams) == 0 {
		return nil
	}
	bodyMap := make(map[string]interface{}, len(fields))
	for name, raw := range fields {
		var value interface{}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if err := decoder.Decode(&value); err != nil {
			return &RequestCapabilityError{Param: "request", Message: "request cannot be checked against channel custom parameters"}
		}
		bodyMap[name] = value
	}
	var field string
	var changed bool
	bodyMap, field, changed, err = ApplyResponsesCustomParameters(bodyMap, customParams, modelName, operation == providerresponse.OperationResponsesInputTokens, applyPreAdd)
	if err != nil {
		return &RequestCapabilityError{Param: "custom_parameter", Message: "channel custom parameters cannot be evaluated"}
	}
	if changed {
		return &RequestCapabilityError{Param: field, Message: "channel custom parameters cannot modify Responses lifecycle fields"}
	}
	for _, ref := range commonresponses.ProtocolResourceReferences(bodyMap, "responses") {
		switch ref.Kind {
		case "file", "conversation":
			continue
		}
		return &RequestCapabilityError{Param: ref.Path, Message: "resource ownership is not implemented for " + ref.Kind}
	}
	return nil
}

func snapshotResponsesLifecycleFields(body map[string]interface{}, lifecycleFields []string) (map[string]responsesLifecycleFieldState, error) {
	snapshot := make(map[string]responsesLifecycleFieldState, len(lifecycleFields))
	for _, field := range lifecycleFields {
		value, present := body[field]
		state := responsesLifecycleFieldState{present: present}
		if present {
			encoded, err := json.Marshal(value)
			if err != nil {
				return nil, err
			}
			state.value = encoded
		}
		snapshot[field] = state
	}
	return snapshot, nil
}

func changedResponsesLifecycleField(before map[string]responsesLifecycleFieldState, body map[string]interface{}, lifecycleFields []string) (string, bool, error) {
	for _, field := range lifecycleFields {
		prior := before[field]
		value, present := body[field]
		if prior.present != present {
			return field, true, nil
		}
		if !present {
			continue
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return "", false, err
		}
		if !bytes.Equal(prior.value, encoded) {
			return field, true, nil
		}
	}
	return "", false, nil
}
