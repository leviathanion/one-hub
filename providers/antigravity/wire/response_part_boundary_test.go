package wire

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestResponsePartBoundariesAndSignatureOwnershipRoundTrip(t *testing.T) {
	for _, tt := range []struct {
		name     string
		events   []string
		want     string
		textSign bool
	}{
		{
			name:   "adjacent thinking parts",
			events: []string{`[{"text":"first","thought":true},{"text":"second","thought":true,"thoughtSignature":"opaque"}]`},
			want:   `[{"text":"first","thought":true},{"text":"second","thought":true,"thoughtSignature":"opaque"}]`,
		},
		{
			name:     "adjacent signed text parts",
			events:   []string{`[{"text":"first"},{"text":"second","thoughtSignature":"opaque"}]`},
			want:     `[{"text":"first"},{"text":"second","thoughtSignature":"opaque"}]`,
			textSign: true,
		},
		{
			name:   "adjacent unsigned text parts",
			events: []string{`[{"text":"first"},{"text":"second"}]`},
			want:   `[{"text":"first"},{"text":"second"}]`,
		},
		{
			name: "thinking continuation keeps preceding part",
			events: []string{
				`[{"text":"first","thought":true},{"text":"sec","thought":true}]`,
				`[{"text":"ond","thought":true,"thoughtSignature":"opaque"}]`,
			},
			want: `[{"text":"first","thought":true},{"text":"second","thought":true,"thoughtSignature":"opaque"}]`,
		},
		{
			name: "text continuation keeps preceding part",
			events: []string{
				`[{"text":"first"},{"text":"sec"}]`,
				`[{"text":"ond"}]`,
			},
			want: `[{"text":"first"},{"text":"second"}]`,
		},
		{
			name:   "continuous text deltas",
			events: []string{`[{"text":"fir"}]`, `[{"text":"st"}]`},
			want:   `[{"text":"first"}]`,
		},
	} {
		for _, dialect := range []Dialect{DialectChat, DialectClaude, DialectResponses} {
			if tt.textSign && dialect == DialectClaude {
				continue
			}
			for _, stream := range []bool{false, true} {
				mode := "aggregate"
				if stream {
					mode = "stream"
				}
				t.Run(tt.name+"/"+string(dialect)+"/"+mode, func(t *testing.T) {
					raw := responseHistoryForRoundTrip(t, dialect, tt.events, stream)
					converted, err := ConvertRequest(dialect, raw, "test")
					if err != nil {
						t.Fatalf("round trip: %v\nhistory=%s", err, raw)
					}
					got := requestParts(t, converted.Body, 0)
					var wantParts, gotParts any
					if err = json.Unmarshal([]byte(tt.want), &wantParts); err != nil {
						t.Fatal(err)
					}
					if err = json.Unmarshal(marshal(got), &gotParts); err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(wantParts, gotParts) {
						t.Fatalf("part boundaries or signature ownership changed:\nwant=%s\ngot=%s", tt.want, marshal(got))
					}
				})
			}
		}
	}
}

func responseHistoryForRoundTrip(t *testing.T, dialect Dialect, events []string, stream bool) json.RawMessage {
	t.Helper()
	d := NewResponseDecoder(&Context{Dialect: dialect, Model: "test"}, "id", stream)
	var frames [][]byte
	for _, parts := range events {
		frames = append(frames, pushResponse(t, d, `{"candidates":[{"content":{"role":"model","parts":`+parts+`}}]}`)...)
	}
	frames = append(frames, pushResponse(t, d, `{"candidates":[{"finishReason":"STOP"}]}`)...)
	tail, body, err := d.Finish()
	if err != nil {
		t.Fatal(err)
	}
	frames = append(frames, tail...)
	if stream {
		body = assembleResponseHistoryFrames(t, dialect, frames)
	}
	output, err := object(body, "response")
	if err != nil {
		t.Fatal(err)
	}
	request := map[string]any{"model": "test"}
	switch dialect {
	case DialectChat:
		var choices []struct {
			Message json.RawMessage `json:"message"`
		}
		if err = json.Unmarshal(output["choices"], &choices); err != nil || len(choices) != 1 {
			t.Fatalf("choices=%s err=%v", output["choices"], err)
		}
		request["messages"] = []any{choices[0].Message}
	case DialectClaude:
		request["max_tokens"] = 100
		request["messages"] = []any{map[string]any{"role": "assistant", "content": output["content"]}}
	case DialectResponses:
		request["store"] = false
		request["input"] = output["output"]
	}
	return marshal(request)
}

// 按客户端可见事件组装历史，确保流式内容边界也参与往返验证。
func assembleResponseHistoryFrames(t *testing.T, dialect Dialect, frames [][]byte) []byte {
	t.Helper()
	message := fields{"role": marshal("assistant")}
	var toolCalls []fields
	var blocks []fields
	var doneItems []json.RawMessage
	var completed json.RawMessage
	toolArguments := map[int]string{}
	chatFinished := false
	for _, event := range frameObjects(t, frames) {
		switch dialect {
		case DialectChat:
			var choices []struct {
				Delta        fields          `json:"delta"`
				FinishReason json.RawMessage `json:"finish_reason"`
			}
			if err := json.Unmarshal(event["choices"], &choices); err != nil {
				t.Fatal(err)
			}
			for _, choice := range choices {
				for _, field := range []string{"content", "reasoning_content"} {
					if value, ok := choice.Delta[field]; ok {
						message[field] = marshal(stringValue(message[field]) + stringValue(value))
					}
				}
				if raw, ok := choice.Delta["tool_calls"]; ok {
					var calls []fields
					if err := json.Unmarshal(raw, &calls); err != nil {
						t.Fatal(err)
					}
					for _, call := range calls {
						var index int
						if err := json.Unmarshal(call["index"], &index); err != nil || index != len(toolCalls) {
							t.Fatalf("unexpected tool call index: %s", marshal(call))
						}
						delete(call, "index")
						toolCalls = append(toolCalls, call)
					}
				}
				if value, ok := choice.Delta["extra_content"]; ok {
					if chatFinished || hasJSONValue(choice.FinishReason) {
						t.Fatal("continuation state must arrive before the terminal chunk")
					}
					message["extra_content"] = value
				}
				if hasJSONValue(choice.FinishReason) {
					chatFinished = true
				}
			}
		case DialectClaude:
			var index int
			switch stringValue(event["type"]) {
			case "content_block_start":
				if err := json.Unmarshal(event["index"], &index); err != nil || index != len(blocks) {
					t.Fatalf("unexpected block start: %s", marshal(event))
				}
				var block fields
				if err := json.Unmarshal(event["content_block"], &block); err != nil {
					t.Fatal(err)
				}
				blocks = append(blocks, block)
			case "content_block_delta":
				if err := json.Unmarshal(event["index"], &index); err != nil || index < 0 || index >= len(blocks) {
					t.Fatalf("delta without block: %s", marshal(event))
				}
				var delta fields
				if err := json.Unmarshal(event["delta"], &delta); err != nil {
					t.Fatal(err)
				}
				if stringValue(delta["type"]) == "input_json_delta" {
					toolArguments[index] += stringValue(delta["partial_json"])
				}
				for _, field := range []string{"text", "thinking", "signature"} {
					if value, ok := delta[field]; ok {
						blocks[index][field] = marshal(stringValue(blocks[index][field]) + stringValue(value))
					}
				}
			case "content_block_stop":
				if err := json.Unmarshal(event["index"], &index); err != nil || index < 0 || index >= len(blocks) {
					t.Fatalf("stop without block: %s", marshal(event))
				}
				if args, ok := toolArguments[index]; ok {
					if !json.Valid([]byte(args)) {
						t.Fatalf("invalid complete tool input: %s", args)
					}
					blocks[index]["input"] = json.RawMessage(args)
				}
			}
		case DialectResponses:
			switch stringValue(event["type"]) {
			case "response.output_item.done":
				doneItems = append(doneItems, event["item"])
			case "response.completed":
				completed = event["response"]
			}
		}
	}
	switch dialect {
	case DialectChat:
		if len(toolCalls) > 0 {
			message["tool_calls"] = marshal(toolCalls)
		}
		return marshal(map[string]any{"choices": []any{map[string]any{"message": message}}})
	case DialectClaude:
		return marshal(map[string]any{"content": blocks})
	default:
		var snapshot struct {
			Output []json.RawMessage `json:"output"`
		}
		if err := json.Unmarshal(completed, &snapshot); err != nil {
			t.Fatalf("missing completed response: %v", err)
		}
		if !reflect.DeepEqual(doneItems, snapshot.Output) {
			t.Fatalf("streamed items differ from completed snapshot:\nitems=%s\nsnapshot=%s", marshal(doneItems), marshal(snapshot.Output))
		}
		return completed
	}
}
