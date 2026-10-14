package wire

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func responseEnvelope(inner string) []byte { return []byte(`{"response":` + inner + `}`) }

func pushResponse(t *testing.T, d *ResponseDecoder, inner string) [][]byte {
	t.Helper()
	frames, err := d.Push(responseEnvelope(inner))
	if err != nil {
		t.Fatal(err)
	}
	return frames
}

func frameObjects(t *testing.T, frames [][]byte) []map[string]json.RawMessage {
	t.Helper()
	objects := []map[string]json.RawMessage{}
	for _, frame := range frames {
		for _, line := range bytes.Split(frame, []byte("\n")) {
			if !bytes.HasPrefix(line, []byte("data: ")) || bytes.Equal(line, []byte("data: [DONE]")) {
				continue
			}
			var value map[string]json.RawMessage
			if err := json.Unmarshal(line[6:], &value); err != nil {
				t.Fatal(err)
			}
			objects = append(objects, value)
		}
	}
	return objects
}

func TestNativeResponsePreservesInnerWireAndFutureParts(t *testing.T) {
	inner := `{ "candidates":[{"index":0,"content":{"parts":[{"futureUnion":{"n":9007199254740993},"thoughtSignature":"raw"}]},"finishReason":"STOP"}],"future":null }`
	d := NewNativeDecoder(1)
	raw, err := d.Push(responseEnvelope(inner))
	if err != nil || string(raw) != inner {
		t.Fatalf("raw=%s err=%v", raw, err)
	}
	if err = d.Finish(); err != nil {
		t.Fatal(err)
	}
}

func TestNativeFutureCandidateShapesAreDeliveredWithoutInventingTerminal(t *testing.T) {
	for _, inner := range []string{
		`{"candidates":{"futureUnion":[1,2]}}`,
		`{"candidates":[{"index":0,"finishReason":"STOP"},{"index":{"future":1},"finishReason":"STOP"}]}`,
		`{"candidates":[{"index":0,"finishReason":"STOP"},"future-candidate"]}`,
		`{"candidates":[{"index":0,"finishReason":"STOP"},{"index":1,"finishReason":{"future":"STOP"}}]}`,
	} {
		t.Run(inner, func(t *testing.T) {
			d := NewNativeDecoder(1)
			raw, err := d.Push(responseEnvelope(inner))
			if err != nil || string(raw) != inner {
				t.Fatalf("native future value was rejected or changed: %s %v", raw, err)
			}
			if !errors.Is(d.Finish(), ErrTerminalMissing) {
				t.Fatal("unobservable candidate became successful terminal")
			}
			_, err = d.Push(responseEnvelope(`{"candidates":[{"index":0,"finishReason":"STOP"}]}`))
			if err != nil || !errors.Is(d.Finish(), ErrTerminalMissing) {
				t.Fatal("later known candidate erased unknown candidate")
			}
			cross := NewResponseDecoder(&Context{Dialect: DialectChat}, "id", true)
			if _, err = cross.Push(responseEnvelope(inner)); err == nil {
				if _, _, finishErr := cross.Finish(); finishErr == nil {
					t.Fatal("cross conversion accepted unrepresentable candidate")
				}
			}
		})
	}
}

func TestNativeStreamRequiresAllCandidatesAndRetainsTrailingUsage(t *testing.T) {
	d := NewNativeDecoder(2)
	_, err := d.Push(responseEnvelope(`{"candidates":[{"index":0,"finishReason":"STOP"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(d.Finish(), ErrTerminalMissing) {
		t.Fatal("single candidate completed the whole request")
	}
	_, err = d.Push(responseEnvelope(`{"candidates":[{"index":1,"finishReason":"STOP"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	usage := `{"promptTokenCount":3,"candidatesTokenCount":4,"totalTokenCount":7}`
	_, err = d.Push(responseEnvelope(`{"usageMetadata":` + usage + `,"future":{"tail":true}}`))
	if err != nil {
		t.Fatal(err)
	}
	if err = d.Finish(); err != nil || string(d.UsageRaw()) != usage {
		t.Fatalf("usage=%s err=%v", d.UsageRaw(), err)
	}
}

func TestNativeEmptyTruncatedBlockedAndErrorTerminals(t *testing.T) {
	for _, tt := range []struct {
		name    string
		events  []string
		success bool
	}{
		{"empty", nil, false},
		{"truncated", []string{`{"candidates":[{"content":{"parts":[{"text":"partial"}]}}]}`}, false},
		{"blocked", []string{`{"promptFeedback":{"blockReason":"SAFETY"}}`, `{"usageMetadata":{"promptTokenCount":2}}`}, true},
		{"other candidate unfinished", []string{`{"candidates":[{"index":0,"finishReason":"STOP"},{"index":1}]}`}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := NewNativeDecoder(1)
			for _, event := range tt.events {
				if _, err := d.Push(responseEnvelope(event)); err != nil {
					t.Fatal(err)
				}
			}
			if err := d.Finish(); (err == nil) != tt.success {
				t.Fatalf("Finish=%v", err)
			}
		})
	}
	d := NewNativeDecoder(1)
	_, _ = d.Push(responseEnvelope(`{"candidates":[{"finishReason":"STOP"}]}`))
	_, err := d.Push(responseEnvelope(`{"usageMetadata":{"promptTokenCount":5},"error":{"code":503,"message":"tail"}}`))
	var upstream *UpstreamError
	if !errors.As(err, &upstream) || d.Finish() == nil || !strings.Contains(string(d.UsageRaw()), "5") {
		t.Fatalf("tail error lost: %v usage=%s", err, d.UsageRaw())
	}
}

func TestChatStreamMixedContentAndCompleteSignatureBeforeTerminal(t *testing.T) {
	d := NewResponseDecoder(&Context{Dialect: DialectChat, Model: "test", IncludeUsage: true}, "id", true)
	frames := pushResponse(t, d, `{"candidates":[{"content":{"parts":[{"text":"think","thought":true},{"text":"answer"},{"functionCall":{"name":"lookup","args":{"x":1}}}]}}]}`)
	frames = append(frames, pushResponse(t, d, `{"candidates":[{"content":{"parts":[{"thoughtSignature":"late"}]},"finishReason":"STOP"}]}`)...)
	if bytes.Contains(bytes.Join(frames, nil), []byte("[DONE]")) {
		t.Fatal("terminal emitted before EOF")
	}
	pushResponse(t, d, `{"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":3,"thoughtsTokenCount":4,"totalTokenCount":9}}`)
	tail, body, err := d.Finish()
	if err != nil || body != nil {
		t.Fatalf("Finish=%v body=%s", err, body)
	}
	frames = append(frames, tail...)
	wire := string(bytes.Join(frames, nil))
	if !strings.Contains(wire, `"reasoning_content":"think"`) || !strings.Contains(wire, `"content":"answer"`) || strings.Contains(wire, `"tool_calls":null`) {
		t.Fatalf("mixed output=%s", wire)
	}
	if strings.Index(wire, "antigravity_state") > strings.Index(wire, `"finish_reason":"tool_calls"`) {
		t.Fatal("carrier arrived after terminal")
	}
	var state string
	for _, object := range frameObjects(t, frames) {
		var choices []struct {
			Delta struct {
				Extra map[string]string `json:"extra_content"`
			} `json:"delta"`
		}
		_ = json.Unmarshal(object["choices"], &choices)
		for _, choice := range choices {
			if value := choice.Delta.Extra["antigravity_state"]; value != "" {
				state = value
			}
		}
	}
	parts, err := DecodeCarrier(state)
	if err != nil || len(parts) != 4 || parts[0].Kind != "thought" || parts[1].Kind != "text" || parts[2].CallID == "" || parts[3].Kind != "signature" {
		t.Fatalf("carrier=%+v err=%v", parts, err)
	}
	if !strings.Contains(wire, `"completion_tokens":7`) {
		t.Fatalf("thinking usage missing: %s", wire)
	}
}

func TestClaudeToolWaitsForLateSignatureWithoutFabricatedThinking(t *testing.T) {
	d := NewResponseDecoder(&Context{Dialect: DialectClaude, Model: "test"}, "id", true)
	first := pushResponse(t, d, `{"candidates":[{"content":{"parts":[{"functionCall":{"id":"call","name":"lookup","args":{}}}]}}]}`)
	if bytes.Contains(bytes.Join(first, nil), []byte("tool_use")) {
		t.Fatal("tool emitted before signature association closed")
	}
	pushResponse(t, d, `{"candidates":[{"content":{"parts":[{"thoughtSignature":"late"}]},"finishReason":"STOP"}]}`)
	frames, _, err := d.Finish()
	if err != nil {
		t.Fatal(err)
	}
	wire := string(bytes.Join(frames, nil))
	if strings.Contains(wire, `"type":"thinking"`) || !strings.Contains(wire, `"type":"tool_use"`) || !strings.Contains(wire, `"signature":"ag1.`) {
		t.Fatalf("tool output=%s", wire)
	}
}

func TestClaudeStreamStartsWithRequiredUsageAndRetainsTrailingCounts(t *testing.T) {
	for _, tt := range []struct {
		name   string
		usage  string
		input  int
		output int
	}{
		{name: "no initial usage"},
		{name: "initial input only", usage: `,"usageMetadata":{"promptTokenCount":8}`, input: 8},
		{name: "initial output only", usage: `,"usageMetadata":{"candidatesTokenCount":2}`, output: 2},
		{name: "initial complete usage", usage: `,"usageMetadata":{"promptTokenCount":8,"cachedContentTokenCount":3,"candidatesTokenCount":2,"thoughtsTokenCount":1}`, input: 5, output: 3},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := NewResponseDecoder(&Context{Dialect: DialectClaude, Model: "test"}, "id", true)
			frames := pushResponse(t, d, `{"candidates":[{"content":{"parts":[{"text":"answer"}]}}]`+tt.usage+`}`)
			objects := frameObjects(t, frames)
			if len(objects) < 2 || string(objects[0]["type"]) != `"message_start"` {
				t.Fatalf("content did not start streaming: %q", frames)
			}
			var message struct {
				Usage *struct {
					InputTokens  *int `json:"input_tokens"`
					OutputTokens *int `json:"output_tokens"`
				} `json:"usage"`
			}
			if err := json.Unmarshal(objects[0]["message"], &message); err != nil {
				t.Fatal(err)
			}
			if message.Usage == nil || message.Usage.InputTokens == nil || message.Usage.OutputTokens == nil || *message.Usage.InputTokens != tt.input || *message.Usage.OutputTokens != tt.output {
				t.Fatalf("message_start requires valid initial counts: %s", objects[0]["message"])
			}
			if tt.usage == "" && len(d.UsageRaw()) != 0 {
				t.Fatalf("initial counts became billing evidence: %s", d.UsageRaw())
			}
			finalUsage := `{"promptTokenCount":11,"candidatesTokenCount":7}`
			pushResponse(t, d, `{"candidates":[{"finishReason":"STOP"}],"usageMetadata":`+finalUsage+`}`)
			tail, _, err := d.Finish()
			if err != nil {
				t.Fatal(err)
			}
			objects = frameObjects(t, tail)
			var deltaUsage json.RawMessage
			for _, object := range objects {
				if string(object["type"]) == `"message_delta"` {
					deltaUsage = object["usage"]
				}
			}
			if string(deltaUsage) != `{"input_tokens":11,"output_tokens":7}` || string(d.UsageRaw()) != finalUsage || string(objects[len(objects)-1]["type"]) != `"message_stop"` {
				t.Fatalf("trailing usage or terminal lost: frames=%q usage=%s", tail, d.UsageRaw())
			}
		})
	}
}

func TestCrossProtocolFunctionCallArguments(t *testing.T) {
	for _, dialect := range []Dialect{DialectChat, DialectClaude, DialectResponses} {
		for _, stream := range []bool{false, true} {
			mode := "aggregate"
			if stream {
				mode = "stream"
			}
			for _, tt := range []struct {
				name string
				args string
				want string
			}{
				{name: "omitted", want: `{}`},
				{name: "empty object", args: `,"args":{}`, want: `{}`},
				{name: "object", args: `,"args":{"n":9007199254740993}`, want: `{"n":9007199254740993}`},
			} {
				t.Run(string(dialect)+"/"+mode+"/"+tt.name, func(t *testing.T) {
					d := NewResponseDecoder(&Context{Dialect: dialect}, "id", stream)
					frames := pushResponse(t, d, `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"get_time"`+tt.args+`}}]},"finishReason":"STOP"}]}`)
					tail, body, err := d.Finish()
					if err != nil {
						t.Fatal(err)
					}
					if stream {
						body = bytes.Join(append(frames, tail...), nil)
					}
					quotedArgs, _ := json.Marshal(tt.want)
					want := `"arguments":` + string(quotedArgs)
					if dialect == DialectClaude {
						want = `"input":` + tt.want
						if stream {
							want = `"partial_json":` + string(quotedArgs)
						}
					} else if dialect == DialectResponses && stream {
						want = `"delta":` + string(quotedArgs)
					}
					if !bytes.Contains(body, []byte(want)) || !bytes.Contains(body, []byte(`"name":"get_time"`)) {
						t.Fatalf("function call lost: want=%s body=%s", want, body)
					}
				})
			}
			for _, args := range []string{`null`, `[]`, `"invalid"`, `1`, `true`} {
				t.Run(string(dialect)+"/"+mode+"/invalid "+args, func(t *testing.T) {
					d := NewResponseDecoder(&Context{Dialect: dialect}, "id", stream)
					frames, err := d.Push(responseEnvelope(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"get_time","args":` + args + `}}]},"finishReason":"STOP"}]}`))
					if err == nil || len(frames) != 0 {
						t.Fatalf("invalid arguments accepted: frames=%q err=%v", frames, err)
					}
					if frames, body, err := d.Finish(); err == nil || len(frames) != 0 || len(body) != 0 {
						t.Fatalf("invalid call completed: frames=%q body=%s err=%v", frames, body, err)
					}
				})
			}
		}
	}
}

func TestNativeResponsePreservesOmittedFunctionArguments(t *testing.T) {
	inner := `{ "candidates":[{"content":{"parts":[{"functionCall":{"name":"get_time"}}]},"finishReason":"STOP"}] }`
	d := NewNativeDecoder(1)
	raw, err := d.Push(responseEnvelope(inner))
	if err != nil || string(raw) != inner {
		t.Fatalf("native response changed: raw=%s err=%v", raw, err)
	}
	if err = d.Finish(); err != nil {
		t.Fatal(err)
	}
}

func TestResponsesContinuationAndCompletedSnapshot(t *testing.T) {
	d := NewResponseDecoder(&Context{Dialect: DialectResponses, Model: "test"}, "resp", true)
	frames := pushResponse(t, d, `{"candidates":[{"content":{"parts":[{"text":"reason","thought":true,"thoughtSignature":"sig"},{"text":"answer"}]},"finishReason":"STOP"}]}`)
	tail, _, err := d.Finish()
	if err != nil {
		t.Fatal(err)
	}
	frames = append(frames, tail...)
	wire := string(bytes.Join(frames, nil))
	if strings.Index(wire, "encrypted_content") > strings.Index(wire, "event: response.completed") {
		t.Fatal("continuation state after response.completed")
	}
	objects := frameObjects(t, frames)
	last := objects[len(objects)-1]
	var response struct {
		Status string                       `json:"status"`
		Output []map[string]json.RawMessage `json:"output"`
	}
	if err = json.Unmarshal(last["response"], &response); err != nil {
		t.Fatal(err)
	}
	if response.Status != "completed" || len(response.Output) != 3 || !hasJSONValue(response.Output[2]["encrypted_content"]) {
		t.Fatalf("response=%s", last["response"])
	}
}

func TestCrossProtocolPromptBlockContainsNoInventedText(t *testing.T) {
	for _, dialect := range []Dialect{DialectChat, DialectClaude, DialectResponses} {
		t.Run(string(dialect), func(t *testing.T) {
			d := NewResponseDecoder(&Context{Dialect: dialect}, "id", false)
			pushResponse(t, d, `{"promptFeedback":{"blockReason":"SAFETY"},"usageMetadata":{"promptTokenCount":8}}`)
			_, body, err := d.Finish()
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(body, []byte(`"text":`)) || bytes.Contains(body, []byte(`"output_tokens":0`)) {
				t.Fatalf("invented output=%s", body)
			}
		})
	}
}

func TestUnrepresentableOutputAndTailErrorNeverComplete(t *testing.T) {
	for _, event := range []string{
		`{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"AA=="}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3}}`,
		`{"error":{"code":503,"message":"failed"},"usageMetadata":{"promptTokenCount":3}}`,
	} {
		d := NewResponseDecoder(&Context{Dialect: DialectChat}, "id", true)
		if _, err := d.Push(responseEnvelope(event)); err == nil {
			t.Fatal("unrepresentable/error response accepted")
		}
		if frames, _, err := d.Finish(); err == nil || len(frames) != 0 {
			t.Fatalf("failure completed: frames=%q err=%v", frames, err)
		}
		if string(d.UsageRaw()) != `{"promptTokenCount":3}` {
			t.Fatalf("failure usage lost: %s", d.UsageRaw())
		}
	}
}

func TestUsageSnapshotsDoNotAccumulateOrInventMissingCounts(t *testing.T) {
	d := NewResponseDecoder(&Context{Dialect: DialectChat}, "id", false)
	pushResponse(t, d, `{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":2}}`)
	pushResponse(t, d, `{"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"thoughtsTokenCount":3,"cachedContentTokenCount":4}}`)
	_, body, err := d.Finish()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte(`"completion_tokens":8`)) || !bytes.Contains(body, []byte(`"prompt_tokens":10`)) || bytes.Contains(body, []byte(`"total_tokens":`)) {
		t.Fatalf("usage=%s", body)
	}
	for _, raw := range []string{`{}`, `{"promptTokenCount":null}`, `{"candidatesTokenCount":"invalid"}`} {
		if got := usageFields([]byte(raw), DialectChat); got != nil {
			t.Fatalf("invalid fields became zero: %s => %+v", raw, got)
		}
	}
	invalidComponent := usageFields([]byte(`{"promptTokenCount":10,"toolUsePromptTokenCount":null,"candidatesTokenCount":5,"thoughtsTokenCount":-1}`), DialectChat)
	if invalidComponent != nil {
		t.Fatalf("invalid components produced totals: %+v", invalidComponent)
	}
}

func TestDuplicateToolCallAndConflictingTerminalsFailExplicitly(t *testing.T) {
	for _, events := range [][]string{
		{`{"candidates":[{"content":{"parts":[{"functionCall":{"id":"same","name":"f","args":{}}},{"functionCall":{"id":"same","name":"f","args":{}}}]}}]}`},
		{`{"candidates":[{"finishReason":"STOP"}]}`, `{"candidates":[{"finishReason":"MAX_TOKENS"}]}`},
	} {
		d := NewResponseDecoder(&Context{Dialect: DialectChat}, "id", true)
		var err error
		for _, event := range events {
			_, err = d.Push(responseEnvelope(event))
			if err != nil {
				break
			}
		}
		if err == nil {
			t.Fatal("ambiguous output accepted")
		}
		if frames, _, err := d.Finish(); err == nil || len(frames) != 0 {
			t.Fatalf("failed stream completed: %q %v", frames, err)
		}
	}
}

func TestChatStreamingDoesNotRetainAlreadyDeliveredText(t *testing.T) {
	d := NewResponseDecoder(&Context{Dialect: DialectChat}, "id", true)
	chunk := `{"candidates":[{"content":{"parts":[{"text":"` + strings.Repeat("x", 1<<20) + `"}]}}]}`
	for i := 0; i < 20; i++ {
		pushResponse(t, d, chunk)
	}
	if d.retained > 4096 || d.candidates[0].current.text.Len() != 0 {
		t.Fatalf("delivered text retained: %d", d.retained)
	}
	pushResponse(t, d, `{"candidates":[{"finishReason":"STOP"}]}`)
	if _, _, err := d.Finish(); err != nil {
		t.Fatal(err)
	}
}

func TestResponsesAggregationBudgetIsEnforced(t *testing.T) {
	d := NewResponseDecoder(&Context{Dialect: DialectResponses}, "id", false)
	d.retained = MaxRetainedBytes - 1024
	_, err := d.Push(responseEnvelope(`{"candidates":[{"content":{"parts":[{"text":"` + strings.Repeat("x", 2048) + `"}]}}]}`))
	if err == nil {
		t.Fatal("response exceeded retained budget")
	}
}

func TestSignedResponseHistoryRoundTripsOrderAndToolAssociation(t *testing.T) {
	parts := `[{"text":"思考","thought":true,"thoughtSignature":"thinking"},{"text":"before"},{"functionCall":{"id":"call","name":"lookup","args":{"n":9007199254740993}},"thoughtSignature":"tool"},{"thoughtSignature":"separate"},{"text":"after"}]`
	for _, dialect := range []Dialect{DialectChat, DialectClaude, DialectResponses} {
		t.Run(string(dialect), func(t *testing.T) {
			d := NewResponseDecoder(&Context{Dialect: dialect, Model: "test"}, "id", false)
			pushResponse(t, d, `{"candidates":[{"content":{"role":"model","parts":`+parts+`},"finishReason":"STOP"}]}`)
			_, body, err := d.Finish()
			if err != nil {
				t.Fatal(err)
			}
			var output map[string]json.RawMessage
			_ = json.Unmarshal(body, &output)
			request := map[string]any{"model": "test"}
			switch dialect {
			case DialectChat:
				var choices []struct {
					Message json.RawMessage `json:"message"`
				}
				_ = json.Unmarshal(output["choices"], &choices)
				request["messages"] = []any{map[string]any{"role": "user", "content": "hello"}, choices[0].Message, map[string]any{"role": "tool", "tool_call_id": "call", "content": "done"}}
			case DialectClaude:
				request["max_tokens"] = 100
				request["messages"] = []any{map[string]any{"role": "user", "content": "hello"}, map[string]any{"role": "assistant", "content": output["content"]}, map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "call", "content": "done"}}}}
			case DialectResponses:
				var items []json.RawMessage
				_ = json.Unmarshal(output["output"], &items)
				input := []any{map[string]any{"role": "user", "content": "hello"}}
				for _, item := range items {
					input = append(input, item)
				}
				input = append(input, map[string]any{"type": "function_call_output", "call_id": "call", "output": "done"})
				request["input"] = input
				request["store"] = false
			}
			raw, _ := json.Marshal(request)
			converted, err := ConvertRequest(dialect, raw, "test")
			if err != nil {
				t.Fatalf("round trip: %v\nresponse=%s", err, body)
			}
			var restored struct {
				Contents []struct {
					Role  string            `json:"role"`
					Parts []json.RawMessage `json:"parts"`
				} `json:"contents"`
			}
			if err = json.Unmarshal(converted.Body, &restored); err != nil {
				t.Fatal(err)
			}
			if len(restored.Contents) != 3 || restored.Contents[1].Role != "model" {
				t.Fatalf("contents=%s", converted.Body)
			}
			var want []json.RawMessage
			_ = json.Unmarshal([]byte(parts), &want)
			got := restored.Contents[1].Parts
			if len(got) != len(want) {
				t.Fatalf("parts=%s", converted.Body)
			}
			for i := range want {
				var a, b any
				decoder := json.NewDecoder(bytes.NewReader(want[i]))
				decoder.UseNumber()
				_ = decoder.Decode(&a)
				decoder = json.NewDecoder(bytes.NewReader(got[i]))
				decoder.UseNumber()
				_ = decoder.Decode(&b)
				wa, _ := json.Marshal(a)
				ga, _ := json.Marshal(b)
				if !bytes.Equal(wa, ga) {
					t.Fatalf("part %d: want=%s got=%s", i, wa, ga)
				}
			}
		})
	}
}
