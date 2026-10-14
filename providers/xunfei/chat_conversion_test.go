package xunfei

import (
	"context"
	"encoding/json"
	"testing"

	"one-api/types"
)

func TestXunfeiUnaryFunctionArgumentsSerializeAsStrings(t *testing.T) {
	for _, mode := range []string{"tool_calls", "function_call"} {
		for _, tc := range []struct {
			name      string
			arguments string
			want      string
		}{
			{
				name:      "object",
				arguments: `{"city":"北京","future":{"count":9007199254740993,"scale":1e+09,"value":null}}`,
				want:      `{"city":"北京","future":{"count":9007199254740993,"scale":1e+09,"value":null}}`,
			},
			{
				name:      "json_string",
				arguments: `"{ \"city\": \"北京\" }"`,
				want:      `{ "city": "北京" }`,
			},
			{
				name:      "opaque_string",
				arguments: `"unvalidated arguments"`,
				want:      "unvalidated arguments",
			},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				request := &types.ChatCompletionRequest{Model: "spark"}
				if mode == "tool_calls" {
					request.Tools = []*types.ChatCompletionTool{{Type: "function"}}
				}
				handler := &xunfeiHandler{Request: request}
				stream := &xunfeiFakeStream{
					dataChan: make(chan XunfeiChatResponse, 1),
					errChan:  make(chan error, 1),
					closed:   make(chan struct{}),
				}
				line := []byte(`{"header":{"code":0,"sid":"call_a"},"payload":{"choices":{"status":2,"text":[{"function_call":{"name":"weather","arguments":` + tc.arguments + `,"future":null}}]}}}`)
				handler.handlerNotStream(&line, stream.dataChan, stream.errChan)
				response, apiErr := handler.convertToChatOpenai(context.Background(), stream)
				if apiErr != nil {
					t.Fatal(apiErr)
				}
				encoded, err := json.Marshal(response)
				if err != nil {
					t.Fatal(err)
				}

				// 按客户端的字符串类型解析输出，避免宽松的共享解码器掩盖 wire 类型错误。
				type functionCall struct {
					Name      string          `json:"name"`
					Arguments string          `json:"arguments"`
					Future    json.RawMessage `json:"future"`
				}
				var output struct {
					Choices []struct {
						FinishReason string `json:"finish_reason"`
						Message      struct {
							FunctionCall *functionCall `json:"function_call"`
							ToolCalls    []struct {
								ID       string        `json:"id"`
								Function *functionCall `json:"function"`
							} `json:"tool_calls"`
						} `json:"message"`
					} `json:"choices"`
				}
				if err := json.Unmarshal(encoded, &output); err != nil {
					t.Fatalf("客户端无法解析函数参数：%v；响应：%s", err, encoded)
				}
				if len(output.Choices) != 1 || output.Choices[0].FinishReason != mode {
					t.Fatalf("完成原因错误：%s", encoded)
				}
				message := output.Choices[0].Message
				function := message.FunctionCall
				if mode == "tool_calls" {
					if function != nil || len(message.ToolCalls) != 1 || message.ToolCalls[0].ID != "call_a" {
						t.Fatalf("工具调用结构错误：%s", encoded)
					}
					function = message.ToolCalls[0].Function
				} else if len(message.ToolCalls) != 0 {
					t.Fatalf("旧版函数调用包含工具调用：%s", encoded)
				}
				if function == nil || function.Name != "weather" || function.Arguments != tc.want || string(function.Future) != "null" {
					t.Fatalf("函数参数或扩展字段改变：%s", encoded)
				}
			})
		}
	}
}
