package types

import "one-api/common/jsonobject"

func (v *ChatCompletionToolCallsCustom) UnmarshalJSON(data []byte) error {
	type wire ChatCompletionToolCallsCustom
	var decoded wire
	projection, extra, err := jsonobject.DecodeProjection(data, &decoded)
	if err != nil {
		return err
	}
	*v = ChatCompletionToolCallsCustom(decoded)
	v.wireProjection, v.ExtraFields = projection, extra
	return nil
}

func (v ChatCompletionToolCallsCustom) MarshalJSON() ([]byte, error) {
	type wire ChatCompletionToolCallsCustom
	return jsonobject.MarshalProjection(wire(v), v.ExtraFields, v.wireProjection)
}

func (v *ChatCompletionToolCalls) UnmarshalJSON(data []byte) error {
	type wire ChatCompletionToolCalls
	var decoded wire
	projection, extra, err := jsonobject.DecodeProjection(data, &decoded)
	if err != nil {
		return err
	}
	*v = ChatCompletionToolCalls(decoded)
	v.wireProjection, v.ExtraFields = projection, extra
	return nil
}

func (v ChatCompletionToolCalls) MarshalJSON() ([]byte, error) {
	type wire ChatCompletionToolCalls
	return jsonobject.MarshalProjection(wire(v), v.ExtraFields, v.wireProjection)
}

func (v *ChatCompletionMessage) UnmarshalJSON(data []byte) error {
	type wire ChatCompletionMessage
	var decoded wire
	projection, extra, err := jsonobject.DecodeProjection(data, &decoded)
	if err != nil {
		return err
	}
	*v = ChatCompletionMessage(decoded)
	v.wireProjection, v.ExtraFields = projection, extra
	return nil
}

func (v ChatCompletionMessage) MarshalJSON() ([]byte, error) {
	type wire ChatCompletionMessage
	return jsonobject.MarshalProjection(wire(v), v.ExtraFields, v.wireProjection)
}

func (v *ChatMessageImageURL) UnmarshalJSON(data []byte) error {
	type wire ChatMessageImageURL
	var decoded wire
	projection, extra, err := jsonobject.DecodeProjection(data, &decoded)
	if err != nil {
		return err
	}
	*v = ChatMessageImageURL(decoded)
	v.wireProjection, v.ExtraFields = projection, extra
	return nil
}

func (v ChatMessageImageURL) MarshalJSON() ([]byte, error) {
	type wire ChatMessageImageURL
	return jsonobject.MarshalProjection(wire(v), v.ExtraFields, v.wireProjection)
}

func (v *ChatMessagePart) UnmarshalJSON(data []byte) error {
	type wire ChatMessagePart
	var decoded wire
	projection, extra, err := jsonobject.DecodeProjection(data, &decoded)
	if err != nil {
		return err
	}
	*v = ChatMessagePart(decoded)
	v.wireProjection, v.ExtraFields = projection, extra
	return nil
}

func (v ChatMessagePart) MarshalJSON() ([]byte, error) {
	type wire ChatMessagePart
	return jsonobject.MarshalProjection(wire(v), v.ExtraFields, v.wireProjection)
}

func (v *ChatCompletionFunction) UnmarshalJSON(data []byte) error {
	type wire ChatCompletionFunction
	var decoded wire
	projection, extra, err := jsonobject.DecodeProjection(data, &decoded)
	if err != nil {
		return err
	}
	*v = ChatCompletionFunction(decoded)
	v.wireProjection, v.ExtraFields = projection, extra
	return nil
}

func (v ChatCompletionFunction) MarshalJSON() ([]byte, error) {
	type wire ChatCompletionFunction
	return jsonobject.MarshalProjection(wire(v), v.ExtraFields, v.wireProjection)
}

func (v *ChatCompletionStreamChoiceDelta) UnmarshalJSON(data []byte) error {
	type wire ChatCompletionStreamChoiceDelta
	var decoded wire
	projection, extra, err := jsonobject.DecodeProjection(data, &decoded)
	if err != nil {
		return err
	}
	*v = ChatCompletionStreamChoiceDelta(decoded)
	v.wireProjection, v.ExtraFields = projection, extra
	return nil
}

func (v ChatCompletionStreamChoiceDelta) MarshalJSON() ([]byte, error) {
	type wire ChatCompletionStreamChoiceDelta
	return jsonobject.MarshalProjection(wire(v), v.ExtraFields, v.wireProjection)
}

func (v ChatCompletionToolCallsFunction) MarshalJSON() ([]byte, error) {
	type wire ChatCompletionToolCallsFunction
	return jsonobject.MarshalProjection(wire(v), v.ExtraFields, v.wireProjection)
}
