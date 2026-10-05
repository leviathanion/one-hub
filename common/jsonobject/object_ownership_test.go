package jsonobject

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

func TestParseOwnsRawAndFieldBytes(t *testing.T) {
	// Several decoder buffers are needed; earlier RawMessages must remain owned
	// even after decoding subsequent fields or reusing the caller's input.
	first := `{"text":"` + strings.Repeat("first", 1024) + `"}`
	last := `{"text":"` + strings.Repeat("last", 2048) + `","n":12345678901234567890}`
	raw := []byte(" \n {\"first\": " + first + ", \"last\": " + last + "} \t")
	original := bytes.Clone(raw)
	object, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	for i := range raw {
		raw[i] = '?'
	}
	if !bytes.Equal(object.Raw, original) {
		t.Fatal("raw object aliases the caller's input")
	}
	if string(object.Fields["first"]) != first || string(object.Fields["last"]) != last {
		t.Fatal("field bytes changed after later decoding or caller input reuse")
	}

	object.Fields["first"][0] = '['
	if !bytes.Equal(object.Raw, original) || string(object.Fields["last"]) != last {
		t.Fatal("fields must not alias Raw or another field")
	}
}

func TestCloneRetainsIndependentRawFieldsAndOrder(t *testing.T) {
	object, err := Parse([]byte(`{"first":{"n":12345678901234567890},"last":true}`))
	if err != nil {
		t.Fatal(err)
	}
	clone := object.Clone()
	clone.Raw[0] = '['
	clone.Fields["first"][0] = '['
	clone.Order[0] = "changed"
	delete(clone.Fields, "last")
	if string(object.Raw) != `{"first":{"n":12345678901234567890},"last":true}` ||
		string(object.Fields["first"]) != `{"n":12345678901234567890}` ||
		string(object.Fields["last"]) != "true" ||
		!reflect.DeepEqual(object.Order, []string{"first", "last"}) {
		t.Fatal("editing a Clone changed the source object")
	}
}
