package wire

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestChatUnsignedHistoryPreservesContentOrder(t *testing.T) {
	call := `{"functionCall":{"id":"call","name":"lookup","args":{"n":9007199254740993}}}`
	secondCall := `{"functionCall":{"id":"second","name":"lookup","args":{}}}`
	for _, tt := range []struct {
		name    string
		parts   string
		carrier bool
	}{
		{"text around tool", `[{"text":"before"},` + call + `,{"text":"after"}]`, true},
		{"text after tool", `[` + call + `,{"text":"after"}]`, true},
		{"thought within text", `[{"text":"before"},{"text":"思考","thought":true},{"text":"after"}]`, true},
		{"text between tools", `[` + call + `,{"text":"between"},` + secondCall + `]`, true},
		{"empty text after tool", `[` + call + `,{"text":""}]`, true},
		{"plain text", `[{"text":"answer"}]`, false},
		{"native chat order", `[{"text":"思考","thought":true},{"text":"answer"},` + call + `,` + secondCall + `]`, false},
	} {
		for _, stream := range []bool{false, true} {
			mode := "aggregate"
			if stream {
				mode = "stream"
			}
			t.Run(tt.name+"/"+mode, func(t *testing.T) {
				raw := responseHistoryForRoundTrip(t, DialectChat, []string{tt.parts}, stream)
				present, err := InspectCarrier(raw, DialectChat)
				if err != nil || present != tt.carrier {
					t.Fatalf("carrier=%v want=%v err=%v history=%s", present, tt.carrier, err, raw)
				}
				converted := mustRequest(t, DialectChat, string(raw))
				parts := requestParts(t, converted.Body, 0)
				gotRaw, err := json.Marshal(parts)
				if err != nil {
					t.Fatal(err)
				}
				var got, want any
				for _, value := range []struct {
					input  string
					target *any
				}{{string(gotRaw), &got}, {tt.parts, &want}} {
					decoder := json.NewDecoder(strings.NewReader(value.input))
					decoder.UseNumber()
					if err := decoder.Decode(value.target); err != nil {
						t.Fatal(err)
					}
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("history order changed: got=%s want=%s", gotRaw, tt.parts)
				}
			})
		}
	}
}
