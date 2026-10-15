package wire

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestClaudeSummaryDisplayKeepsComputationControls(t *testing.T) {
	for _, mode := range []string{"enabled", "adaptive"} {
		t.Run(mode, func(t *testing.T) {
			thinking := `"type":"` + mode + `"`
			want := `{"contents":[],"generationConfig":{"thinkingConfig":{"thinkingBudget":-1,"includeThoughts":true}}}`
			if mode == "enabled" {
				thinking += `,"budget_tokens":2048`
				want = `{"contents":[],"generationConfig":{"thinkingConfig":{"thinkingBudget":2048,"includeThoughts":true}}}`
			}
			base := `{"messages":[],"thinking":{` + thinking + `}}`
			before := mustRequest(t, DialectClaude, base)
			assertSameJSON(t, []byte(want), before.Body)
			for _, display := range []string{`"summarized"`, `null`} {
				after := mustRequest(t, DialectClaude, `{"messages":[],"thinking":{`+thinking+`,"display":`+display+`}}`)
				assertSameJSON(t, []byte(want), after.Body)
				if !bytes.Equal(before.Body, after.Body) {
					t.Fatalf("display changed existing computation/default: before=%s after=%s", before.Body, after.Body)
				}
			}
		})
	}
	for _, output := range []string{`{}`, `{"effort":null}`} {
		req := mustRequest(t, DialectClaude, `{"messages":[],"output_config":`+output+`}`)
		if bytes.Contains(req.Body, []byte("thinkingConfig")) {
			t.Fatalf("absent effort enabled thinking: %s", req.Body)
		}
	}
}

func TestClaudeNullableCacheAndBufferedToolsPreserveRequest(t *testing.T) {
	base := `{"messages":[
		{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"f","input":{"cache_control":{"type":"business-data"},"n":9007199254740993}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":[{"type":"text","text":"done"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"Yg=="}}]},{"type":"text","text":"next"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"Yg=="}}]}],
		"system":[{"type":"text","text":"instructions"}],
		"tools":[{"name":"f","input_schema":{"type":"object","properties":{"cache_control":{"const":{"ttl":"business-data"}}}}}]}`
	// Only protocol-owned positions change. Identically named business/schema keys stay opaque.
	withNulls := strings.NewReplacer(
		`"type":"text","text":`, `"type":"text","cache_control":null,"text":`,
		`"type":"image","source":`, `"type":"image","cache_control":null,"source":`,
		`"type":"tool_use","id":`, `"type":"tool_use","cache_control":null,"id":`,
		`"type":"tool_result","tool_use_id":`, `"type":"tool_result","cache_control":null,"tool_use_id":`,
		`"name":"f","input_schema":`, `"name":"f","cache_control":null,"eager_input_streaming":false,"input_schema":`,
	).Replace(base)
	before, after := mustRequest(t, DialectClaude, base), mustRequest(t, DialectClaude, withNulls)
	assertSameJSON(t, before.Body, after.Body)
}

func TestCrossProtocolContractFailuresNameSpecificRequirement(t *testing.T) {
	cases := []struct {
		name, body, param, reason string
	}{
		{"hidden summary", `"thinking":{"type":"enabled","budget_tokens":2048,"display":"omitted"}`, "thinking.display", "签名续接"},
		{"unknown display", `"thinking":{"type":"adaptive","display":"future"}`, "thinking.display", "显示选项"},
		{"disabled display", `"thinking":{"type":"disabled","display":"summarized"}`, "thinking.display", "enabled"},
		{"adaptive explicit budget", `"thinking":{"type":"adaptive","budget_tokens":2048,"display":"summarized"}`, "thinking.budget_tokens", "覆盖"},
		{"disabled explicit budget", `"thinking":{"type":"disabled","budget_tokens":2048}`, "thinking.budget_tokens", "覆盖"},
		{"effort", `"output_config":{"effort":"high"}`, "output_config.effort", "经过验证"},
		{"effort type", `"output_config":{"effort":3}`, "output_config.effort", "字符串"},
		{"other output field", `"output_config":{"format":{"type":"json_schema"}}`, "output_config.format", "尚未支持"},
		{"default cache ttl", `"system":[{"type":"text","text":"x","cache_control":{"type":"ephemeral"}}]`, "system[0].cache_control", "默认 5m"},
		{"explicit cache ttl", `"tools":[{"name":"f","input_schema":{},"cache_control":{"type":"ephemeral","ttl":"1h"}}]`, "tools[0].cache_control.ttl", "缓存计费"},
		{"unknown cache type", `"system":[{"type":"text","text":"x","cache_control":{"type":"persistent"}}]`, "system[0].cache_control.type", "缓存类型"},
		{"empty cache", `"system":[{"type":"text","text":"x","cache_control":{}}]`, "system[0].cache_control.type", "缓存类型"},
		{"unknown cache ttl", `"system":[{"type":"text","text":"x","cache_control":{"type":"ephemeral","ttl":"2h"}}]`, "system[0].cache_control.ttl", "缓存期限"},
		{"unknown cache member", `"system":[{"type":"text","text":"x","cache_control":{"type":"ephemeral","future":true}}]`, "system[0].cache_control.future", "尚未支持"},
		{"eager", `"tools":[{"name":"f","input_schema":{},"eager_input_streaming":true}]`, "tools[0].eager_input_streaming", "迟到签名"},
		{"eager default", `"tools":[{"name":"f","input_schema":{},"eager_input_streaming":null}]`, "tools[0].eager_input_streaming", "beta headers"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ConvertRequest(DialectClaude, []byte(`{"messages":[],`+tc.body+`}`), "any-model")
			var failure *CapabilityError
			if !errors.As(err, &failure) || failure.Param != tc.param || !strings.Contains(failure.Message, tc.reason) {
				t.Fatalf("got %v, want %s (%s)", err, tc.param, tc.reason)
			}
		})
	}
}

func TestClaudeToolResultCacheInstructionIsNotBusinessJSON(t *testing.T) {
	for _, ttl := range []string{"5m", "1h"} {
		body := `{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"f","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":[{"type":"text","text":"done","cache_control":{"type":"ephemeral","ttl":"` + ttl + `"}}]}]}]}`
		_, err := ConvertRequest(DialectClaude, []byte(body), "model")
		var failure *CapabilityError
		if !errors.As(err, &failure) || failure.Param != "messages[1].content[0].content[0].cache_control.ttl" {
			t.Fatalf("cache instruction silently became tool data: %v", err)
		}
	}
}

func TestClaudeToolResultCacheConsumptionRejectsAmbiguousBlock(t *testing.T) {
	request := func(block string) []byte {
		return []byte(`{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"f","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":[` + block + `]}]}]}`)
	}
	for _, block := range []string{
		`{"type":"text","text":"before","text":"after","cache_control":null}`,
		`{"type":"future","type":"text","text":"done","cache_control":null}`,
		`{"type":"text","text":"done","cache_control":{"type":"ephemeral","ttl":"1h"},"cache_control":null}`,
		`{"type":"text","text":"done","future":1,"future":2,"cache_control":null}`,
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"Yg=="},"cache_control":{"type":"ephemeral","ttl":"1h"},"cache_control":null}`,
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"YQ=="},"source":{"type":"base64","media_type":"image/png","data":"Yg=="},"cache_control":null}`,
		`{"type":"future","type":"image","source":{"type":"base64","media_type":"image/png","data":"Yg=="},"cache_control":null}`,
	} {
		_, err := ConvertRequest(DialectClaude, request(block), "model")
		var failure *CapabilityError
		if !errors.As(err, &failure) || failure.Param != "messages[1].content[0].content[0]" || !strings.Contains(failure.Message, "duplicate") {
			t.Fatalf("ambiguous block was rewritten: %s: %v", block, err)
		}
	}
	// Only the owned block is interpreted. Business subtrees remain raw, even
	// when they use the same field names or have their own duplicate keys.
	block := `{"type":"text","text":"done","cache_control":null,"business":{"x":1,"x":2,"n":9007199254740993,"cache_control":{"type":"business-data"}}}`
	converted, err := ConvertRequest(DialectClaude, request(block), "model")
	if err != nil {
		t.Fatal(err)
	}
	want := `"business":{"x":1,"x":2,"n":9007199254740993,"cache_control":{"type":"business-data"}}`
	if !bytes.Contains(converted.Body, []byte(want)) {
		t.Fatalf("opaque business data changed: %s", converted.Body)
	}
}

func TestUnsupportedFieldSelectionIsStable(t *testing.T) {
	for i := 0; i < 100; i++ {
		_, err := ConvertRequest(DialectClaude, []byte(`{"messages":[],"z_future":1,"a_future":2}`), "model")
		var failure *CapabilityError
		if !errors.As(err, &failure) || failure.Param != "request.a_future" {
			t.Fatalf("unstable unsupported field: %v", err)
		}
	}
	// The same future fields remain untouched on the native path.
	raw := `{"contents":[],"z_future":1,"a_future":2}`
	if got := mustRequest(t, DialectGemini, raw); string(got.Body) != raw {
		t.Fatalf("native request changed: %s", got.Body)
	}
}

func TestChatAccumulatedToolIndexMustMatchHistoryPosition(t *testing.T) {
	base := `{"messages":[{"role":"assistant","tool_calls":[{"id":"a","type":"function","function":{"name":"f","arguments":"{}"}},{"id":"b","type":"function","function":{"name":"f","arguments":"{}"}}]}]}`
	indexed := strings.ReplaceAll(strings.ReplaceAll(base, `"id":"a"`, `"index":0,"id":"a"`), `"id":"b"`, `"index":1,"id":"b"`)
	assertSameJSON(t, mustRequest(t, DialectChat, base).Body, mustRequest(t, DialectChat, indexed).Body)
	for _, value := range []string{"0", "-1", "null", `"1"`, "2", "1.5"} {
		bad := strings.Replace(indexed, `"index":1`, `"index":`+value, 1)
		_, err := ConvertRequest(DialectChat, []byte(bad), "model")
		var failure *CapabilityError
		if !errors.As(err, &failure) || failure.Param != "messages[0].tool_calls[1].index" {
			t.Fatalf("inconsistent index accepted: %s, %v", value, err)
		}
	}
}
