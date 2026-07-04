package jsonobject

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Object preserves a top-level JSON object as the request serialization truth
// while still allowing protocol-owned fields to be patched explicitly.
type Object struct {
	Raw    json.RawMessage
	Fields map[string]json.RawMessage
	Order  []string
}

const maxObjectDepth = 64

func Parse(raw []byte) (*Object, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, errors.New("request body is required")
	}
	if err := rejectExcessiveDepth(trimmed, maxObjectDepth); err != nil {
		return nil, err
	}

	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := token.(json.Delim)
	if !ok || delim != '{' {
		return nil, errors.New("request body must be a JSON object")
	}

	fields := make(map[string]json.RawMessage)
	order := make([]string, 0)
	for decoder.More() {
		token, err = decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, errors.New("object key must be a string")
		}
		if _, exists := fields[key]; exists {
			return nil, fmt.Errorf("duplicate top-level JSON key %q", key)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode field %q: %w", key, err)
		}
		fields[key] = append(json.RawMessage(nil), bytes.TrimSpace(value)...)
		order = append(order, key)
	}

	token, err = decoder.Token()
	if err != nil {
		return nil, err
	}
	delim, ok = token.(json.Delim)
	if !ok || delim != '}' {
		return nil, errors.New("request body must be a JSON object")
	}
	if err := rejectTrailingJSON(decoder); err != nil {
		return nil, err
	}

	return &Object{
		Raw:    append(json.RawMessage(nil), raw...),
		Fields: fields,
		Order:  order,
	}, nil
}

func rejectExcessiveDepth(raw []byte, maxDepth int) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()

	depth := 0
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			continue
		}
		switch delim {
		case '{', '[':
			depth++
			if depth > maxDepth {
				return fmt.Errorf("JSON nesting exceeds %d levels", maxDepth)
			}
		case '}', ']':
			if depth > 0 {
				depth--
			}
		}
	}
}

func rejectTrailingJSON(decoder *json.Decoder) error {
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err == io.EOF {
		return nil
	} else if err != nil {
		return err
	}
	if strings.TrimSpace(string(trailing)) != "" {
		return errors.New("request body must not contain trailing JSON")
	}
	return nil
}

func (o *Object) Clone() *Object {
	if o == nil {
		return nil
	}
	clone := &Object{
		Raw:    append(json.RawMessage(nil), o.Raw...),
		Fields: make(map[string]json.RawMessage, len(o.Fields)),
		Order:  append([]string(nil), o.Order...),
	}
	for key, value := range o.Fields {
		clone.Fields[key] = append(json.RawMessage(nil), value...)
	}
	return clone
}

func (o *Object) SetRaw(name string, value json.RawMessage) error {
	if o == nil || strings.TrimSpace(name) == "" {
		return nil
	}
	value = bytes.TrimSpace(value)
	if !json.Valid(value) {
		return fmt.Errorf("field %q contains invalid JSON", name)
	}
	if o.Fields == nil {
		o.Fields = make(map[string]json.RawMessage)
	}
	if _, exists := o.Fields[name]; !exists {
		o.Order = append(o.Order, name)
	}
	o.Fields[name] = append(json.RawMessage(nil), value...)
	o.Raw = nil
	return nil
}

func (o *Object) SetJSON(name string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return o.SetRaw(name, raw)
}

func (o *Object) Delete(name string) {
	if o == nil || o.Fields == nil {
		return
	}
	delete(o.Fields, name)
	o.Raw = nil
}

func (o *Object) MarshalJSON() ([]byte, error) {
	if o == nil {
		return nil, errors.New("json object is nil")
	}
	if len(o.Raw) > 0 {
		return append([]byte(nil), o.Raw...), nil
	}
	if len(o.Fields) == 0 {
		return []byte("{}"), nil
	}
	return marshalFieldsInOrder(o.Fields, o.Order)
}

func marshalFieldsInOrder(fields map[string]json.RawMessage, order []string) ([]byte, error) {
	var buffer bytes.Buffer
	buffer.WriteByte('{')

	wrote := false
	seen := make(map[string]struct{}, len(fields))
	writeField := func(name string) error {
		value, ok := fields[name]
		if !ok {
			return nil
		}
		if !json.Valid(value) {
			return fmt.Errorf("field %q contains invalid JSON", name)
		}
		if wrote {
			buffer.WriteByte(',')
		}
		encodedName, err := json.Marshal(name)
		if err != nil {
			return err
		}
		buffer.Write(encodedName)
		buffer.WriteByte(':')
		buffer.Write(bytes.TrimSpace(value))
		wrote = true
		seen[name] = struct{}{}
		return nil
	}

	for _, name := range order {
		if _, exists := seen[name]; exists {
			continue
		}
		if err := writeField(name); err != nil {
			return nil, err
		}
	}
	for name := range fields {
		if _, exists := seen[name]; exists {
			continue
		}
		if err := writeField(name); err != nil {
			return nil, err
		}
	}

	buffer.WriteByte('}')
	return buffer.Bytes(), nil
}
