package requestctx

import (
	"net/http"
	"strings"
)

type FieldState int

const (
	FieldMissing FieldState = iota
	FieldEmpty
	FieldPresent
	FieldInvalid
	FieldMultiple
)

type HeaderField struct {
	CanonicalName string
	Values        []string
}

type HeaderSnapshot struct {
	Fields map[string]HeaderField
}

type HeaderValue struct {
	Name   string
	State  FieldState
	Value  string
	Values []string
}

func NewHeaderSnapshot(headers http.Header) HeaderSnapshot {
	fields := make(map[string]HeaderField, len(headers))
	for name, values := range headers {
		canonical := http.CanonicalHeaderKey(name)
		key := strings.ToLower(canonical)
		cloned := append([]string(nil), values...)
		if existing, ok := fields[key]; ok {
			existing.Values = append(existing.Values, cloned...)
			fields[key] = existing
			continue
		}
		fields[key] = HeaderField{
			CanonicalName: canonical,
			Values:        cloned,
		}
	}
	return HeaderSnapshot{Fields: fields}
}

func (s HeaderSnapshot) Values(name string) []string {
	if s.Fields == nil {
		return nil
	}
	field, ok := s.Fields[strings.ToLower(strings.TrimSpace(name))]
	if !ok {
		return nil
	}
	return append([]string(nil), field.Values...)
}

func (s HeaderSnapshot) Singleton(name string, valid func(string) bool) HeaderValue {
	key := strings.ToLower(strings.TrimSpace(name))
	field, ok := s.Fields[key]
	canonical := http.CanonicalHeaderKey(name)
	if ok && field.CanonicalName != "" {
		canonical = field.CanonicalName
	}
	if !ok || len(field.Values) == 0 {
		return HeaderValue{Name: canonical, State: FieldMissing}
	}
	if len(field.Values) > 1 {
		return HeaderValue{Name: canonical, State: FieldMultiple, Values: append([]string(nil), field.Values...)}
	}
	value := strings.TrimSpace(field.Values[0])
	if value == "" {
		return HeaderValue{Name: canonical, State: FieldEmpty, Values: append([]string(nil), field.Values...)}
	}
	if valid != nil && !valid(value) {
		return HeaderValue{Name: canonical, State: FieldInvalid, Value: value, Values: append([]string(nil), field.Values...)}
	}
	return HeaderValue{Name: canonical, State: FieldPresent, Value: value, Values: append([]string(nil), field.Values...)}
}

func (s HeaderSnapshot) HasNonEmpty(name string) bool {
	return s.Singleton(name, nil).State == FieldPresent
}
