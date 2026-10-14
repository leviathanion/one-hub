package wire

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestUsageOutputCountsUseProvableCandidateEvidence(t *testing.T) {
	for _, dialect := range []Dialect{DialectChat, DialectClaude, DialectResponses} {
		for _, tt := range []struct {
			name   string
			raw    string
			output int64
			known  bool
		}{
			{name: "explicit candidates", raw: `{"promptTokenCount":10,"candidatesTokenCount":3,"thoughtsTokenCount":99,"totalTokenCount":112}`, output: 102, known: true},
			{name: "explicit partial usage", raw: `{"candidatesTokenCount":3,"thoughtsTokenCount":99}`, output: 102, known: true},
			{name: "omitted zero candidates", raw: `{"promptTokenCount":10,"thoughtsTokenCount":99,"totalTokenCount":109}`, output: 99, known: true},
			{name: "omitted zero with tool usage", raw: `{"promptTokenCount":10,"toolUsePromptTokenCount":2,"thoughtsTokenCount":99,"totalTokenCount":111}`, output: 99, known: true},
			{name: "omitted zero without thoughts", raw: `{"promptTokenCount":10,"totalTokenCount":10}`, known: true},
			{name: "missing total", raw: `{"promptTokenCount":10,"thoughtsTokenCount":99}`},
			{name: "missing prompt", raw: `{"thoughtsTokenCount":99,"totalTokenCount":99}`},
			{name: "unexplained remainder", raw: `{"promptTokenCount":10,"thoughtsTokenCount":99,"totalTokenCount":110}`},
			{name: "null candidates", raw: `{"promptTokenCount":10,"candidatesTokenCount":null,"thoughtsTokenCount":99,"totalTokenCount":109}`},
			{name: "invalid thoughts", raw: `{"promptTokenCount":10,"thoughtsTokenCount":-1,"totalTokenCount":9}`},
			{name: "contradictory candidate detail", raw: `{"promptTokenCount":10,"thoughtsTokenCount":99,"totalTokenCount":109,"candidatesTokensDetails":[{"modality":"TEXT","tokenCount":1}]}`},
			{name: "unaccounted tool detail", raw: `{"promptTokenCount":10,"thoughtsTokenCount":99,"totalTokenCount":109,"toolUsePromptTokensDetails":[{"modality":"TEXT","tokenCount":2}]}`},
			{name: "cache exceeds prompt", raw: `{"promptTokenCount":10,"thoughtsTokenCount":99,"totalTokenCount":109,"cachedContentTokenCount":11}`},
		} {
			t.Run(string(dialect)+"/"+tt.name, func(t *testing.T) {
				field := "output_tokens"
				if dialect == DialectChat {
					field = "completion_tokens"
				}
				fields := usageFields([]byte(tt.raw), dialect)
				output, known := fields[field]
				if known != tt.known || known && output != tt.output {
					t.Fatalf("输出用量 = %v, 已知 = %t，预期 %d, %t；usage = %+v", output, known, tt.output, tt.known, fields)
				}
			})
		}
	}
}

func TestCrossProtocolThinkingOnlyUsagePreservesOutputCounts(t *testing.T) {
	const rawUsage = `{"promptTokenCount":10,"thoughtsTokenCount":99,"totalTokenCount":109}`
	for _, dialect := range []Dialect{DialectChat, DialectClaude, DialectResponses} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", dialect, stream), func(t *testing.T) {
				d := NewResponseDecoder(&Context{Dialect: dialect, IncludeUsage: true}, "id", stream)
				frames := pushResponse(t, d, `{"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"MAX_TOKENS"}],"usageMetadata":`+rawUsage+`}`)
				tail, body, err := d.Finish()
				if err != nil {
					t.Fatal(err)
				}
				var response map[string]json.RawMessage
				if stream {
					for _, object := range frameObjects(t, append(frames, tail...)) {
						if dialect == DialectResponses && string(object["type"]) == `"response.incomplete"` {
							if err := json.Unmarshal(object["response"], &response); err != nil {
								t.Fatal(err)
							}
						} else if _, ok := object["usage"]; ok {
							response = object
						}
					}
				} else if err := json.Unmarshal(body, &response); err != nil {
					t.Fatal(err)
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(response["usage"], &fields); err != nil {
					t.Fatalf("缺失终态用量: %v，response = %s", err, response)
				}
				input, output := "input_tokens", "output_tokens"
				if dialect == DialectChat {
					input, output = "prompt_tokens", "completion_tokens"
				}
				for field, want := range map[string]int64{input: 10, output: 99} {
					var count int64
					if err := json.Unmarshal(fields[field], &count); err != nil {
						t.Fatalf("缺失 %s: %v，usage = %s", field, err, response["usage"])
					}
					if count != want {
						t.Fatalf("%s = %d，预期 %d；usage = %s", field, count, want, response["usage"])
					}
				}
				if string(d.UsageRaw()) != rawUsage {
					t.Fatalf("原始计费用量被改写: %s", d.UsageRaw())
				}
			})
		}
	}
}
