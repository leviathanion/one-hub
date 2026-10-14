package wire

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func mustRequest(t *testing.T, dialect Dialect, input string) *Request {
	t.Helper()
	request, err := ConvertRequest(dialect, []byte(input), "future-model")
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func requestParts(t *testing.T, raw []byte, index int) []json.RawMessage {
	t.Helper()
	o, err := object(raw, "request")
	if err != nil {
		t.Fatal(err)
	}
	contents, err := array(o["contents"], "contents")
	if err != nil {
		t.Fatal(err)
	}
	c, err := object(contents[index], "content")
	if err != nil {
		t.Fatal(err)
	}
	parts, err := array(c["parts"], "parts")
	if err != nil {
		t.Fatal(err)
	}
	return parts
}

func TestNativeGeminiPreservesRawFutureFields(t *testing.T) {
	raw := ` { "contents": {"futureUnion":[900719925474099312345,null]}, "generationConfig":{"candidateCount":"future","maxOutputTokens":-7}, "safetySettings":null,"unknown":{"a":1e8},"thoughtSignature":"unvalidated" } `
	req := mustRequest(t, DialectGemini, raw)
	if !bytes.Equal(req.Body, []byte(raw)) {
		t.Fatalf("native raw changed: %s", req.Body)
	}
	if req.Context.ExpectedCandidates != 1 {
		t.Fatal("malformed observation changed native control")
	}
}

func TestChatSchemaAndArgumentsAreOpaque(t *testing.T) {
	schema := `{"oneOf":[{"minimum":0},{"maximum":10}],"additionalProperties":false,"const":3,"enum":[1,2],"$defs":{"Thing":{"type":"number"}},"$ref":"#/$defs/Thing","future":{"n":900719925474099312345}}`
	req := mustRequest(t, DialectChat, `{"messages":[{"role":"assistant","content":"before","tool_calls":[{"id":"c1","type":"function","function":{"name":"calc","arguments":"{\"value\":5}"}}]}],"tools":[{"type":"function","function":{"name":"calc","parameters":`+schema+`}}]}`)
	o, _ := object(req.Body, "request")
	tools, _ := array(o["tools"], "tools")
	tool, _ := object(tools[0], "tool")
	decls, _ := array(tool["functionDeclarations"], "decls")
	decl, _ := object(decls[0], "decl")
	if string(decl["parametersJsonSchema"]) != schema {
		t.Fatalf("schema changed: %s", decl["parametersJsonSchema"])
	}
	parts := requestParts(t, req.Body, 0)
	if len(parts) != 2 || !bytes.Contains(parts[0], []byte(`"text":"before"`)) || !bytes.Contains(parts[1], []byte(`"value":5`)) {
		t.Fatalf("mixed content lost: %s", req.Body)
	}
}

func TestCrossProtocolRejectsOnlyUnrepresentablePaths(t *testing.T) {
	cases := []struct {
		dialect     Dialect
		body, param string
	}{
		{DialectChat, `{"messages":[{"role":"user","content":"x","future":true}]}`, "messages[0].future"},
		{DialectChat, `{"messages":[{"role":"tool","tool_call_id":"missing","content":"x"}]}`, "messages[0].tool_call_id"},
		{DialectChat, `{"messages":[],"parallel_tool_calls":false}`, "parallel_tool_calls"},
		{DialectClaude, `{"messages":[{"role":"user","content":[{"type":"text","text":"x","cache_control":{"type":"ephemeral"}}]}]}`, "messages[0].content[0].cache_control"},
		{DialectResponses, `{"input":"x"}`, "store"},
		{DialectResponses, `{"store":false,"input":"x","previous_response_id":"r1"}`, "previous_response_id"},
		{DialectResponses, `{"store":false,"input":"x","tools":[{"type":"tool_search","execution":"client"}]}`, "tools[0].type"},
	}
	for _, test := range cases {
		t.Run(string(test.dialect)+test.param, func(t *testing.T) {
			_, err := ConvertRequest(test.dialect, []byte(test.body), "any-new-model")
			var capability *CapabilityError
			if !errors.As(err, &capability) || capability.Param != test.param {
				t.Fatalf("got %v, want %s", err, test.param)
			}
		})
	}
}

func TestClaudeThinkingAndParallelToolResults(t *testing.T) {
	req := mustRequest(t, DialectClaude, `{"thinking":{"type":"enabled","budget_tokens":123},"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"consider","signature":"opaque-original"},{"type":"text","text":"checking"},{"type":"tool_use","id":"a","name":"first","input":{}},{"type":"tool_use","id":"b","name":"second","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"b","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"Yg=="}}]},{"type":"tool_result","tool_use_id":"a","content":[]}]}]}`)
	first := requestParts(t, req.Body, 0)
	if len(first) != 4 || !bytes.Contains(first[0], []byte(`"thoughtSignature":"opaque-original"`)) {
		t.Fatalf("thinking lost: %s", req.Body)
	}
	results := requestParts(t, req.Body, 1)
	if len(results) != 2 || !bytes.Contains(results[0], []byte(`"id":"b"`)) || !bytes.Contains(results[0], []byte(`"name":"second"`)) || !bytes.Contains(results[0], []byte(`"inlineData"`)) || !bytes.Contains(results[1], []byte(`"id":"a"`)) {
		t.Fatalf("parallel association lost: %s", req.Body)
	}
	if bytes.Contains(req.Body, []byte("success")) {
		t.Fatal("fabricated tool success")
	}
}

func TestChatCarrierRestoresInterleavedLayoutWithoutDuplicatingText(t *testing.T) {
	records := []CarrierPart{{Index: 0, Kind: "text", TextLength: 3}, {Index: 1, Kind: "function", CallID: "c1", Signature: marshal("native")}, {Index: 2, Kind: "text", TextLength: 3}, {Index: 3, Kind: "signature", Signature: marshal("late")}}
	state, err := EncodeCarrier(records)
	if err != nil {
		t.Fatal(err)
	}
	decoded, _ := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(state, "ag1."))
	if bytes.Contains(decoded, []byte("before")) {
		t.Fatal("carrier contains body")
	}
	input := marshal(map[string]any{"messages": []any{map[string]any{"role": "assistant", "content": "abcdef", "tool_calls": []any{map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "f", "arguments": "{}"}}}, "extra_content": map[string]any{"antigravity_state": state}}}})
	req := mustRequest(t, DialectChat, string(input))
	parts := requestParts(t, req.Body, 0)
	want := []string{`"text":"abc"`, `"functionCall"`, `"text":"def"`, `"thoughtSignature":"late"`}
	if len(parts) != len(want) {
		t.Fatalf("parts %s", req.Body)
	}
	for i, s := range want {
		if !bytes.Contains(parts[i], []byte(s)) {
			t.Fatalf("part %d = %s", i, parts[i])
		}
	}
}

func TestCarrierRejectsVersionAssociationAndInvalidUTF8Boundaries(t *testing.T) {
	for _, value := range []string{"ag2.AA", "ag1.%%%", "ag1." + base64.RawURLEncoding.EncodeToString([]byte(`[]`))} {
		if _, err := DecodeCarrier(value); err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
	state, err := EncodeCarrier([]CarrierPart{{Index: 0, Kind: "function", CallID: "other", Signature: marshal("opaque")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = restoreCarrier([]json.RawMessage{marshal(map[string]any{"functionCall": map[string]any{"id": "c", "name": "f", "args": map[string]any{}}})}, state, "state"); err == nil {
		t.Fatal("accepted wrong call")
	}
	state, err = EncodeCarrier([]CarrierPart{{Index: 0, Kind: "text", TextLength: 1, Signature: marshal("opaque")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = restoreCarrier([]json.RawMessage{textPart(marshal("中"))}, state, "state"); err == nil {
		t.Fatal("split UTF-8 accepted")
	}
}

func TestThinkingControlsDoNotEnableSummaryImplicitly(t *testing.T) {
	req := mustRequest(t, DialectResponses, `{"store":false,"input":"x","reasoning":{"effort":"high"}}`)
	if bytes.Contains(req.Body, []byte("includeThoughts")) {
		t.Fatal("effort implicitly enables summary")
	}
	req = mustRequest(t, DialectResponses, `{"store":false,"input":"x","reasoning":{"effort":"high","summary":"none"}}`)
	if !bytes.Contains(req.Body, []byte(`"includeThoughts":false`)) {
		t.Fatalf("summary choice lost: %s", req.Body)
	}
}

func TestResponsesCarrierRestoresToolTurn(t *testing.T) {
	state, err := EncodeCarrier([]CarrierPart{{Index: 0, Kind: "thought", TextLength: 3, Signature: marshal("reason")}, {Index: 1, Kind: "function", CallID: "call", Signature: marshal("tool")}})
	if err != nil {
		t.Fatal(err)
	}
	input := marshal(map[string]any{"store": false, "input": []any{map[string]any{"type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": "why"}}, "encrypted_content": state}, map[string]any{"type": "function_call", "call_id": "call", "name": "f", "arguments": "{}"}, map[string]any{"type": "function_call_output", "call_id": "call", "output": ""}}})
	req := mustRequest(t, DialectResponses, string(input))
	parts := requestParts(t, req.Body, 0)
	if len(parts) != 2 || !bytes.Contains(parts[0], []byte(`"thoughtSignature":"reason"`)) || !bytes.Contains(parts[1], []byte(`"thoughtSignature":"tool"`)) {
		t.Fatalf("state lost: %s", req.Body)
	}
	if !bytes.Contains(requestParts(t, req.Body, 1)[0], []byte(`"result":""`)) {
		t.Fatal("empty result changed")
	}
}

func TestInspectCarrierRecognizesOnlyOwnedPositions(t *testing.T) {
	cases := []struct {
		dialect       Dialect
		raw           string
		present, fail bool
	}{
		{DialectChat, `{"messages":[{"role":"assistant","extra_content":{"antigravity_state":null}}]}`, true, true},
		{DialectClaude, `{"messages":[{"content":[{"type":"thinking","thinking":"x","signature":"ag2.AA"}]}]}`, true, true},
		{DialectClaude, `{"messages":[{"content":[{"type":"thinking","thinking":"ag1.bad","signature":"native"}]}]}`, false, false},
		{DialectResponses, `{"input":[{"type":"reasoning","encrypted_content":"ag1.bad"}]}`, true, true},
	}
	for _, test := range cases {
		present, err := InspectCarrier([]byte(test.raw), test.dialect)
		if present != test.present || (err != nil) != test.fail {
			t.Fatalf("%s: %v %v", test.raw, present, err)
		}
	}
}

func TestRemoteImageConversionIsPureAndKeepsURI(t *testing.T) {
	req := mustRequest(t, DialectChat, `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.test/image.png"}}]}]}`)
	if !bytes.Contains(req.Body, []byte(`"fileUri":"https://example.test/image.png"`)) {
		t.Fatalf("URI lost: %s", req.Body)
	}
}

func TestResponsesImageRequiresRepresentableInlineSource(t *testing.T) {
	input := `{"store":false,"input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,aGk="}]}]}`
	request := mustRequest(t, DialectResponses, input)
	if !bytes.Contains(request.Body, []byte(`"inlineData":{"data":"aGk=","mimeType":"image/png"}`)) {
		t.Fatalf("inline image lost: %s", request.Body)
	}
	remote := strings.Replace(input, "data:image/png;base64,aGk=", "https://example.test/image.png", 1)
	_, err := ConvertRequest(DialectResponses, []byte(remote), "future-model")
	var capability *CapabilityError
	if !errors.As(err, &capability) || !strings.HasSuffix(capability.Param, ".image_url") {
		t.Fatalf("remote URI was silently treated as prepared media: %v", err)
	}
}

func TestToolOutputPreservesOpaqueJSONSubtrees(t *testing.T) {
	result := `{"future":[900719925474099312345,null],"items":[{"type":"future","payload":{"x":1e8}}]}`
	req := mustRequest(t, DialectResponses, `{"store":false,"input":[{"type":"function_call","call_id":"c","name":"f","arguments":"{}"},{"type":"function_call_output","call_id":"c","output":`+result+`}]}`)
	part, _ := object(requestParts(t, req.Body, 1)[0], "part")
	call, _ := object(part["functionResponse"], "call")
	response, _ := object(call["response"], "response")
	if string(response["result"]) != result {
		t.Fatalf("opaque JSON result changed: %s", response["result"])
	}
}

func TestFinalizeRequestChecksFinalTargetOutputContract(t *testing.T) {
	cases := []struct {
		name      string
		dialect   Dialect
		body      string
		wantCount int
		param     string
	}{
		{"chat candidates", DialectChat, `{"generationConfig":{"candidateCount":2}}`, 2, ""},
		{"native snake candidates", DialectGemini, `{"generation_config":{"candidate_count":3,"response_modalities":["FUTURE"]},"tools":{"futureUnion":true}}`, 3, ""},
		{"native unknown candidate type", DialectGemini, `{"generationConfig":{"candidateCount":{"future":true}}}`, 1, ""},
		{"claude candidates", DialectClaude, `{"generationConfig":{"candidateCount":2}}`, 2, "generationConfig.candidateCount"},
		{"responses candidates", DialectResponses, `{"generation_config":{"candidate_count":2}}`, 2, "generationConfig.candidateCount"},
		{"chat image", DialectChat, `{"generationConfig":{"responseModalities":["TEXT","IMAGE"]}}`, 1, "generationConfig.responseModalities"},
		{"claude code execution", DialectClaude, `{"tools":[{"codeExecution":{}}]}`, 1, "tools[0].codeExecution"},
		{"responses logprobs", DialectResponses, `{"generation_config":{"response_logprobs":true}}`, 1, "generation_config.response_logprobs"},
		{"opaque sampling", DialectChat, `{"generationConfig":{"temperature":{"future":null},"responseModalities":["TEXT"]},"tools":[{"functionDeclarations":[{"parametersJsonSchema":{"future":900719925474099312345}}]}]}`, 1, ""},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			req := &Request{Body: json.RawMessage(test.body), Context: &Context{Dialect: test.dialect, ExpectedCandidates: 99}}
			err := FinalizeRequest(req)
			if test.param == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				var capability *CapabilityError
				if !errors.As(err, &capability) || capability.Param != test.param {
					t.Fatalf("got %v want %s", err, test.param)
				}
			}
			if req.Context.ExpectedCandidates != test.wantCount {
				t.Fatalf("candidate count = %d", req.Context.ExpectedCandidates)
			}
			if string(req.Body) != test.body {
				t.Fatal("final assessment rewrote target raw")
			}
		})
	}
}
