package claude

import (
	"encoding/json"
	"testing"

	"one-api/types"
)

func decodeI027ChatChunk(t *testing.T, handler *ClaudeStreamHandler, raw string) types.ChatCompletionStreamResponse {
	t.Helper()
	data := make(chan string, 1)
	errors := make(chan error, 1)
	line := []byte(raw)
	handler.HandlerStream(&line, data, errors)
	select {
	case err := <-errors:
		t.Fatalf("Claude stream 映射失败：%v", err)
	case output := <-data:
		var chunk types.ChatCompletionStreamResponse
		if err := json.Unmarshal([]byte(output), &chunk); err != nil {
			t.Fatalf("Chat stream chunk 不是 JSON：%v；raw=%q", err, output)
		}
		return chunk
	default:
		t.Fatal("Claude stream 映射没有输出 chunk")
	}
	return types.ChatCompletionStreamResponse{}
}

func TestI027ClaudeThinkingAndTextUseOneCompletionChoice(t *testing.T) {
	handler := &ClaudeStreamHandler{
		Usage:   &types.Usage{},
		Request: &types.ChatCompletionRequest{Model: "claude-i027"},
		Prefix:  "data: ",
	}

	thinking := decodeI027ChatChunk(t, handler, `data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"plan"}}`)
	text := decodeI027ChatChunk(t, handler, `data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"answer"}}`)
	if len(thinking.Choices) != 1 || thinking.Choices[0].Index != 0 || thinking.Choices[0].Delta.ReasoningContent != "plan" {
		t.Fatalf("thinking block leaked content-block index into completion choice: %+v", thinking)
	}
	if len(text.Choices) != 1 || text.Choices[0].Index != 0 || text.Choices[0].Delta.Content != "answer" {
		t.Fatalf("text block was not projected as choice 0: %+v", text)
	}
}

func TestI027ClaudeInterleavedTextAndToolsKeepDistinctToolIndexes(t *testing.T) {
	handler := &ClaudeStreamHandler{
		Usage:   &types.Usage{},
		Request: &types.ChatCompletionRequest{Model: "claude-i027"},
		Prefix:  "data: ",
	}
	events := []string{
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"first"}}`,
		`data: {"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"second"}}`,
		`data: {"type":"content_block_start","index":3,"content_block":{"type":"tool_use","id":"call_one","name":"lookup"}}`,
		`data: {"type":"content_block_delta","index":3,"delta":{"type":"input_json_delta","partial_json":"{\"a\":"}}`,
		`data: {"type":"content_block_delta","index":3,"delta":{"type":"input_json_delta","partial_json":"1}"}}`,
		`data: {"type":"content_block_delta","index":4,"delta":{"type":"text_delta","text":"between"}}`,
		`data: {"type":"content_block_start","index":5,"content_block":{"type":"tool_use","id":"call_two","name":"search"}}`,
		`data: {"type":"content_block_delta","index":5,"delta":{"type":"input_json_delta","partial_json":"{\"q\":\"x\"}"}}`,
	}
	var chunks []types.ChatCompletionStreamResponse
	for _, event := range events {
		chunks = append(chunks, decodeI027ChatChunk(t, handler, event))
	}

	var toolChunks []types.ChatCompletionStreamChoice
	for _, chunk := range chunks {
		if len(chunk.Choices) != 1 || chunk.Choices[0].Index != 0 {
			t.Fatalf("内容块 index 不能成为 completion choice index：%+v", chunk)
		}
		if len(chunk.Choices[0].Delta.ToolCalls) > 0 {
			toolChunks = append(toolChunks, chunk.Choices[0])
		}
	}
	if len(toolChunks) != 5 {
		t.Fatalf("expected 5 tool chunks (two starts and three argument deltas), got %d: %+v", len(toolChunks), chunks)
	}
	wantIndexes := []int{0, 0, 0, 1, 1}
	wantIDs := []string{"call_one", "", "", "call_two", ""}
	wantNames := []string{"lookup", "", "", "search", ""}
	wantArguments := []string{"", `{"a":`, "1}", "", `{"q":"x"}`}
	for i, choice := range toolChunks {
		tool := choice.Delta.ToolCalls[0]
		if tool.Index != wantIndexes[i] || tool.Id != wantIDs[i] || tool.Function == nil || tool.Function.Name != wantNames[i] || tool.Function.Arguments != wantArguments[i] {
			t.Fatalf("tool chunk %d lost content-block/tool index mapping: got=%+v want index=%d id=%q name=%q args=%q", i, tool, wantIndexes[i], wantIDs[i], wantNames[i], wantArguments[i])
		}
	}
}

func TestI027ClaudeEmptyToolBlockStopEmitsEmptyObjectArguments(t *testing.T) {
	handler := &ClaudeStreamHandler{
		Usage:   &types.Usage{},
		Request: &types.ChatCompletionRequest{Model: "claude-i027"},
		Prefix:  "data: ",
	}
	start := decodeI027ChatChunk(t, handler, `data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_empty","name":"lookup","input":{}}}`)
	if len(start.Choices) != 1 || len(start.Choices[0].Delta.ToolCalls) != 1 || start.Choices[0].Delta.ToolCalls[0].Function == nil || start.Choices[0].Delta.ToolCalls[0].Function.Arguments != "" {
		t.Fatalf("工具起始块应先发送空参数增量：%+v", start)
	}
	stop := decodeI027ChatChunk(t, handler, `data: {"type":"content_block_stop","index":0}`)
	if len(stop.Choices) != 1 || len(stop.Choices[0].Delta.ToolCalls) != 1 || stop.Choices[0].Delta.ToolCalls[0].Index != 0 || stop.Choices[0].Delta.ToolCalls[0].Function == nil || stop.Choices[0].Delta.ToolCalls[0].Function.Arguments != "{}" {
		t.Fatalf("无参数工具 stop 应补完整空对象：%+v", stop)
	}
}

func TestI027ClaudeToolBlockStopDoesNotInventArgumentsForInvalidInput(t *testing.T) {
	for _, test := range []struct {
		name  string
		input string
	}{
		{name: "missing input", input: ""},
		{name: "non-object input", input: `,"input":"invalid"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := &ClaudeStreamHandler{
				Usage:   &types.Usage{},
				Request: &types.ChatCompletionRequest{Model: "claude-i027"},
				Prefix:  "data: ",
			}
			start := `data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_invalid","name":"lookup"` +
				test.input + `}}`
			_ = decodeI027ChatChunk(t, handler, start)
			data := make(chan string, 1)
			errs := make(chan error, 1)
			line := []byte(`data: {"type":"content_block_stop","index":0}`)
			handler.HandlerStream(&line, data, errs)
			select {
			case output := <-data:
				t.Fatalf("非法/缺失 input 不应凭空产生参数对象：%s", output)
			default:
			}
		})
	}
}
