package zhipu

import (
	"encoding/json"
	"testing"

	"one-api/types"
)

func TestStreamPluginsBecomeAssistantContent(t *testing.T) {
	for _, tc := range []struct {
		name string
		call string
		want string
	}{
		{"drawing", `{"type":"drawing_tool","drawing_tool":{"outputs":[{"image":"https://example.com/image.png"}]}}`, "\n![https://example.com/image.png](https://example.com/image.png)\n\n"},
		{"code input", `{"type":"code_interpreter","code_interpreter":{"input":"print(1)"}}`, "```python\n\nprint(1)"},
		{"code output", `{"type":"code_interpreter","code_interpreter":{"outputs":[{"type":"logs","logs":"1"}]}}`, "\n```\n1\n```\n"},
		{"browser", `{"type":"web_browser","web_browser":{"outputs":[{"title":"页面","link":"https://example.com","content":"正文"}]}}`, "\n[页面](https://example.com)\n正文\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := &zhipuStreamHandler{Request: &types.ChatCompletionRequest{Model: "glm-4-alltools"}, Usage: &types.Usage{}}
			line := []byte(`data: {"id":"response","choices":[{"index":0,"delta":{"tool_calls":[` + tc.call + `]}}]}`)
			data := make(chan string, 1)
			errs := make(chan error, 1)
			handler.handlerStream(&line, data, errs)
			select {
			case err := <-errs:
				t.Fatal(err)
			case output := <-data:
				var chunk types.ChatCompletionStreamResponse
				if err := json.Unmarshal([]byte(output), &chunk); err != nil {
					t.Fatal(err)
				}
				if len(chunk.Choices) != 1 || chunk.Choices[0].Delta.Role != "assistant" || chunk.Choices[0].Delta.Content != tc.want {
					t.Fatalf("插件正文转换错误：%s", output)
				}
			default:
				t.Fatal("缺少插件增量输出")
			}
		})
	}
}

func TestToolCallDecodePreservesFunctionFieldsAndReplacesPlugins(t *testing.T) {
	var call ZhipuToolCalls
	if err := json.Unmarshal([]byte(`{"type":"drawing_tool","drawing_tool":{"input":"画图"}}`), &call); err != nil {
		t.Fatal(err)
	}
	if call.DrawingTool == nil || call.DrawingTool.Input != "画图" {
		t.Fatalf("绘图字段未解码：%+v", call)
	}
	if err := json.Unmarshal([]byte(`{"id":"call_a","type":"function","index":0,"function":{"name":"lookup","arguments":"{}","future":null},"future":9007199254740993}`), &call); err != nil {
		t.Fatal(err)
	}
	if call.DrawingTool != nil || call.CodeInterpreter != nil || call.WebBrowser != nil {
		t.Fatalf("复用对象残留旧插件字段：%+v", call)
	}
	delta := (&ZhipuDelta{ToolCalls: []*ZhipuToolCalls{&call}}).ToOpenAIDelta()
	if len(delta.ToolCalls) != 1 || delta.ToolCalls[0].Id != "call_a" || delta.ToolCalls[0].Function.Name != "lookup" || delta.ToolCalls[0].Function.Arguments != "{}" {
		t.Fatalf("函数调用字段丢失：%+v", delta.ToolCalls)
	}
	if string(delta.ToolCalls[0].ExtraFields["future"]) != "9007199254740993" || string(delta.ToolCalls[0].Function.ExtraFields["future"]) != "null" {
		t.Fatalf("函数调用扩展字段丢失：%+v", delta.ToolCalls[0])
	}
}
