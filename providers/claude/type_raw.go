package claude

import "one-api/common/jsonobject"

func (v *ResContent) UnmarshalJSON(data []byte) error {
	type wire ResContent
	var decoded wire
	projection, extra, err := jsonobject.DecodeProjection(data, &decoded)
	if err != nil {
		return err
	}
	*v = ResContent(decoded)
	v.wireProjection, v.ExtraFields = projection, extra
	return nil
}

func (v ResContent) MarshalJSON() ([]byte, error) {
	type wire ResContent
	return jsonobject.MarshalProjection(wire(v), v.ExtraFields, v.wireProjection)
}

func (v *ContentSource) UnmarshalJSON(data []byte) error {
	type wire ContentSource
	var decoded wire
	projection, extra, err := jsonobject.DecodeProjection(data, &decoded)
	if err != nil {
		return err
	}
	*v = ContentSource(decoded)
	v.wireProjection, v.ExtraFields = projection, extra
	return nil
}

func (v ContentSource) MarshalJSON() ([]byte, error) {
	type wire ContentSource
	return jsonobject.MarshalProjection(wire(v), v.ExtraFields, v.wireProjection)
}

func (v *MessageContent) UnmarshalJSON(data []byte) error {
	type wire MessageContent
	var decoded wire
	projection, extra, err := jsonobject.DecodeProjection(data, &decoded)
	if err != nil {
		return err
	}
	*v = MessageContent(decoded)
	v.wireProjection, v.ExtraFields = projection, extra
	return nil
}

func (v MessageContent) MarshalJSON() ([]byte, error) {
	type wire MessageContent
	return jsonobject.MarshalProjection(wire(v), v.ExtraFields, v.wireProjection)
}

func (v *Delta) UnmarshalJSON(data []byte) error {
	type wire Delta
	var decoded wire
	projection, extra, err := jsonobject.DecodeProjection(data, &decoded)
	if err != nil {
		return err
	}
	*v = Delta(decoded)
	v.wireProjection, v.ExtraFields = projection, extra
	return nil
}

func (v Delta) MarshalJSON() ([]byte, error) {
	type wire Delta
	return jsonobject.MarshalProjection(wire(v), v.ExtraFields, v.wireProjection)
}

func (v *ContentBlock) UnmarshalJSON(data []byte) error {
	type wire ContentBlock
	var decoded wire
	projection, extra, err := jsonobject.DecodeProjection(data, &decoded)
	if err != nil {
		return err
	}
	*v = ContentBlock(decoded)
	v.wireProjection, v.ExtraFields = projection, extra
	return nil
}

func (v ContentBlock) MarshalJSON() ([]byte, error) {
	type wire ContentBlock
	return jsonobject.MarshalProjection(wire(v), v.ExtraFields, v.wireProjection)
}
