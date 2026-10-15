package wire

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

func assertSameJSON(t *testing.T, want, got []byte) {
	t.Helper()
	decode := func(raw []byte) any {
		t.Helper()
		d := json.NewDecoder(bytes.NewReader(raw))
		d.UseNumber()
		var v any
		if err := d.Decode(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	if !reflect.DeepEqual(decode(want), decode(got)) {
		t.Fatalf("JSON changed:\nwant=%s\ngot =%s", want, got)
	}
}

func TestNativeSignatureInCarrierNamespaceRoundTrips(t *testing.T) {
	for _, signature := range []string{"ag1.not-a-carrier", "ag2.opaque", "ag100.future", "native-opaque"} {
		for _, stream := range []bool{false, true} {
			parts := `[{"text":"思考","thought":true,"thoughtSignature":` + string(marshal(signature)) + `}]`
			request := responseHistoryForRoundTrip(t, DialectClaude, []string{parts}, stream)
			converted := mustRequest(t, DialectClaude, string(request))
			assertSameJSON(t, []byte(parts), marshal(requestParts(t, converted.Body, 0)))
		}
	}
}

func TestSignedParallelToolsPreservePartsAcrossDeliveryModes(t *testing.T) {
	events := []string{
		`[{"text":"思考","thought":true,"thoughtSignature":"reason"},{"text":"before"},{"functionCall":{"id":"a","name":"lookup","args":{"n":9007199254740993}},"thoughtSignature":"tool-a"}]`,
		`[{"thoughtSignature":"late-a"},{"functionCall":{"id":"b","name":"lookup","args":{"query":"第二个"}},"thoughtSignature":"tool-b"}]`,
		`[{"thoughtSignature":"late-b"},{"text":"after"}]`,
	}
	// Independent expected target parts: no encoder is used to construct this oracle.
	want := `[{"text":"思考","thought":true,"thoughtSignature":"reason"},{"text":"before"},{"functionCall":{"id":"a","name":"lookup","args":{"n":9007199254740993}},"thoughtSignature":"tool-a"},{"thoughtSignature":"late-a"},{"functionCall":{"id":"b","name":"lookup","args":{"query":"第二个"}},"thoughtSignature":"tool-b"},{"thoughtSignature":"late-b"},{"text":"after"}]`
	for _, dialect := range []Dialect{DialectClaude, DialectChat, DialectResponses} {
		for _, stream := range []bool{false, true} {
			raw := responseHistoryForRoundTrip(t, dialect, events, stream)
			converted := mustRequest(t, dialect, string(raw))
			assertSameJSON(t, []byte(want), marshal(requestParts(t, converted.Body, 0)))
		}
	}
}

func TestResponsesStandaloneSignatureRoundTripsWithoutInventedThinking(t *testing.T) {
	for _, stream := range []bool{false, true} {
		parts := `[{"thoughtSignature":"independent"},{"text":"answer"}]`
		raw := responseHistoryForRoundTrip(t, DialectResponses, []string{parts}, stream)
		converted := mustRequest(t, DialectResponses, string(raw))
		assertSameJSON(t, []byte(parts), marshal(requestParts(t, converted.Body, 0)))
		if bytes.Contains(raw, []byte(`"summary_text"`)) {
			t.Fatalf("invented reasoning text: %s", raw)
		}
	}
}
