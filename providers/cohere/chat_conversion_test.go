package cohere

import (
	"encoding/json"
	"fmt"
	"testing"

	"one-api/providers/base"
	"one-api/types"
)

func TestChatResponseToolPlanBecomesContent(t *testing.T) {
	var response ChatResponse
	if err := json.Unmarshal([]byte(`{"id":"response","finish_reason":"TOOL_CALL","message":{"role":"assistant","tool_plan":"查询天气","tool_calls":[{"id":"call_a","type":"function","function":{"name":"weather","arguments":"{}"}}],"future":9007199254740993}}`), &response); err != nil {
		t.Fatal(err)
	}
	if response.Message.ToolPlan != "查询天气" {
		t.Fatalf("tool_plan 未解码：%+v", response.Message)
	}
	provider := &CohereProvider{BaseProvider: base.BaseProvider{Usage: &types.Usage{}}}
	converted, apiErr := ConvertToChatOpenai(provider, &response, &types.ChatCompletionRequest{Model: "command-r"})
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	message := converted.Choices[0].Message
	if message.Role != "assistant" || message.Content != "查询天气" || len(message.ToolCalls) != 1 || message.ToolCalls[0].Function.Name != "weather" {
		t.Fatalf("工具计划或基础消息字段丢失：%+v", message)
	}
	if string(message.ExtraFields["future"]) != "9007199254740993" {
		t.Fatalf("未知字段丢失：%+v", message.ExtraFields)
	}
	if err := json.Unmarshal([]byte(`{"role":"assistant","content":"完成"}`), response.Message); err != nil {
		t.Fatal(err)
	}
	if response.Message.ToolPlan != "" || response.Message.ToChatCompletionMessage().Content != "完成" {
		t.Fatalf("复用对象残留上一条工具计划：%+v", response.Message)
	}
}

func TestStreamToolCallsUseEventIndexIncludingZero(t *testing.T) {
	for _, index := range []int{0, 1} {
		for _, eventType := range []string{"tool-call-start", "tool-call-delta"} {
			t.Run(fmt.Sprintf("%s/%d", eventType, index), func(t *testing.T) {
				handler := &CohereStreamHandler{Request: &types.ChatCompletionRequest{Model: "command-r"}, Usage: &types.Usage{}}
				callJSON := `{"function":{"arguments":"partial","future":null},"future":9007199254740993}`
				if eventType == "tool-call-start" {
					callJSON = `{"id":"call_a","type":"function","function":{"name":"weather","arguments":"","future":null},"future":9007199254740993}`
				}
				line := []byte(fmt.Sprintf(`data: {"type":%q,"index":%d,"delta":{"message":{"tool_calls":%s}}}`, eventType, index, callJSON))
				data := make(chan string, 1)
				errs := make(chan error, 1)
				handler.HandlerStream(&line, data, errs)
				var output string
				select {
				case err := <-errs:
					t.Fatal(err)
				case output = <-data:
				default:
					t.Fatal("缺少工具增量输出")
				}
				var chunk struct {
					Choices []struct {
						Delta struct {
							ToolCalls []map[string]json.RawMessage `json:"tool_calls"`
						} `json:"delta"`
					} `json:"choices"`
				}
				if err := json.Unmarshal([]byte(output), &chunk); err != nil {
					t.Fatal(err)
				}
				if len(chunk.Choices) != 1 || len(chunk.Choices[0].Delta.ToolCalls) != 1 {
					t.Fatalf("工具增量结构错误：%s", output)
				}
				call := chunk.Choices[0].Delta.ToolCalls[0]
				if string(call["index"]) != fmt.Sprint(index) {
					t.Fatalf("事件索引未显式输出：%s", output)
				}
				var function struct {
					Name      string          `json:"name"`
					Arguments string          `json:"arguments"`
					Future    json.RawMessage `json:"future"`
				}
				if err := json.Unmarshal(call["function"], &function); err != nil {
					t.Fatal(err)
				}
				if string(call["future"]) != "9007199254740993" || string(function.Future) != "null" {
					t.Fatalf("扩展字段改变：%s", output)
				}
				if eventType == "tool-call-start" {
					if string(call["id"]) != `"call_a"` || string(call["type"]) != `"function"` || function.Name != "weather" || function.Arguments != "" {
						t.Fatalf("工具起始字段改变：%s", output)
					}
				} else if call["id"] != nil || call["type"] != nil || function.Name != "" || function.Arguments != "partial" {
					t.Fatalf("工具参数增量改变：%s", output)
				}
			})
		}
	}
}
