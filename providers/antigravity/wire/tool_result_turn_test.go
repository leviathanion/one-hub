package wire

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

func TestParallelToolResultsKeepConversationTurns(t *testing.T) {
	const firstCalls = `{"role":"model","parts":[{"functionCall":{"id":"a","name":"first","args":{}}},{"functionCall":{"id":"b","name":"second","args":{}}}]}`
	const firstResults = `{"role":"user","parts":[{"functionResponse":{"id":"b","name":"second","response":{"result":{"future":[900719925474099312345,null]}}}},{"functionResponse":{"id":"a","name":"first","response":{"result":""}}}]}`
	const secondCalls = `{"role":"model","parts":[{"functionCall":{"id":"c","name":"first","args":{}}},{"functionCall":{"id":"d","name":"second","args":{}}}]}`
	const secondResults = `{"role":"user","parts":[{"functionResponse":{"id":"c","name":"first","response":{"result":"third"}}},{"functionResponse":{"id":"d","name":"second","response":{"result":null}}}]}`
	const nextUser = `{"role":"user","content":"continue"}`
	const finalMessages = `{"role":"assistant","content":"done"},{"role":"user","content":"follow up"}`
	tests := []struct {
		dialect    Dialect
		prefix     string
		firstTurn  string
		secondTurn string
	}{
		{
			dialect: DialectChat,
			prefix:  `{"messages":[`,
			firstTurn: `{"role":"assistant","tool_calls":[{"id":"a","type":"function","function":{"name":"first","arguments":"{}"}},{"id":"b","type":"function","function":{"name":"second","arguments":"{}"}}]},
				{"role":"tool","tool_call_id":"b","content":{"future":[900719925474099312345,null]}},
				{"role":"tool","tool_call_id":"a","content":""}`,
			secondTurn: `{"role":"assistant","tool_calls":[{"id":"c","type":"function","function":{"name":"first","arguments":"{}"}},{"id":"d","type":"function","function":{"name":"second","arguments":"{}"}}]},
				{"role":"tool","tool_call_id":"c","content":"third"},
				{"role":"tool","tool_call_id":"d","content":null}`,
		},
		{
			dialect: DialectResponses,
			prefix:  `{"store":false,"input":[`,
			firstTurn: `{"type":"function_call","call_id":"a","name":"first","arguments":"{}"},
				{"type":"function_call","call_id":"b","name":"second","arguments":"{}"},
				{"type":"function_call_output","call_id":"b","output":{"future":[900719925474099312345,null]}},
				{"type":"function_call_output","call_id":"a","output":""}`,
			secondTurn: `{"type":"function_call","call_id":"c","name":"first","arguments":"{}"},
				{"type":"function_call","call_id":"d","name":"second","arguments":"{}"},
				{"type":"function_call_output","call_id":"c","output":"third"},
				{"type":"function_call_output","call_id":"d","output":null}`,
		},
	}
	for _, test := range tests {
		t.Run(string(test.dialect), func(t *testing.T) {
			cases := []struct{ name, suffix, wantSuffix string }{
				{"results at end", "", ""},
				{"assistant and user after results", "," + finalMessages, `,{"role":"model","parts":[{"text":"done"}]},{"role":"user","parts":[{"text":"follow up"}]}`},
				{"another tool turn", "," + test.secondTurn, "," + secondCalls + "," + secondResults},
				{"user before another tool turn", "," + nextUser + "," + test.secondTurn, `,{"role":"user","parts":[{"text":"continue"}]},` + secondCalls + "," + secondResults},
			}
			for _, scenario := range cases {
				t.Run(scenario.name, func(t *testing.T) {
					request := mustRequest(t, test.dialect, test.prefix+test.firstTurn+scenario.suffix+`]}`)
					body, err := object(request.Body, "request")
					if err != nil {
						t.Fatal(err)
					}
					want := `[` + firstCalls + "," + firstResults + scenario.wantSuffix + `]`
					decode := func(raw []byte) any {
						t.Helper()
						decoder := json.NewDecoder(bytes.NewReader(raw))
						decoder.UseNumber()
						var value any
						if err := decoder.Decode(&value); err != nil {
							t.Fatal(err)
						}
						return value
					}
					if !reflect.DeepEqual(decode(body["contents"]), decode([]byte(want))) {
						t.Fatalf("conversation turns changed:\n got %s\nwant %s", body["contents"], want)
					}
				})
			}
		})
	}
}

func TestResponsesToolResultsPreserveFollowingSignedReasoningTurn(t *testing.T) {
	state, err := EncodeCarrier([]CarrierPart{
		{Index: 0, Kind: "thought", TextLength: 3, Signature: marshal("reason")},
		{Index: 1, Kind: "function", CallID: "c", Signature: marshal("tool")},
	})
	if err != nil {
		t.Fatal(err)
	}
	input := `{"store":false,"input":[
		{"type":"function_call","call_id":"a","name":"first","arguments":"{}"},
		{"type":"function_call","call_id":"b","name":"second","arguments":"{}"},
		{"type":"function_call_output","call_id":"a","output":"first result"},
		{"type":"function_call_output","call_id":"b","output":"second result"},
		{"type":"reasoning","summary":[{"type":"summary_text","text":"why"}],"encrypted_content":` + string(marshal(state)) + `},
		{"type":"function_call","call_id":"c","name":"third","arguments":"{}"},
		{"type":"function_call_output","call_id":"c","output":"third result"}]}`
	request := mustRequest(t, DialectResponses, input)
	body, err := object(request.Body, "request")
	if err != nil {
		t.Fatal(err)
	}
	contents, err := array(body["contents"], "contents")
	if err != nil {
		t.Fatal(err)
	}
	if len(contents) != 4 || len(requestParts(t, request.Body, 1)) != 2 || len(requestParts(t, request.Body, 3)) != 1 {
		t.Fatalf("tool result turns changed: %s", request.Body)
	}
	parts := requestParts(t, request.Body, 2)
	if len(parts) != 2 || !bytes.Contains(parts[0], []byte(`"thoughtSignature":"reason"`)) || !bytes.Contains(parts[1], []byte(`"thoughtSignature":"tool"`)) {
		t.Fatalf("signed reasoning crossed tool result boundary: %s", request.Body)
	}
}
