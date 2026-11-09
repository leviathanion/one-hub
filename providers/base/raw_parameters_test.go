package base

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

func TestRawCustomParametersApplyOnlyAtConfiguredStage(t *testing.T) {
	for _, preAdd := range []bool{false, true} {
		raw := []byte(`{ "value":1, "opaque": { "future":1e+3 } }`)
		params := map[string]interface{}{"pre_add": preAdd, "overwrite": true, "value": 2}
		untouched, err := ApplyRawCustomParams(raw, params, "model", !preAdd)
		if err != nil || !bytes.Equal(raw, untouched) {
			t.Fatalf("wrong stage changed raw: body=%s err=%v", untouched, err)
		}
		changed, err := ApplyRawCustomParams(raw, params, "model", preAdd)
		if err != nil || !bytes.Contains(changed, []byte(`"value":2`)) || !bytes.Contains(changed, []byte(`{ "future":1e+3 }`)) {
			t.Fatalf("configured stage lost raw subtree or skipped rule: body=%s err=%v", changed, err)
		}
	}
}

func TestRemoveParametersThroughArrays(t *testing.T) {
	for _, tc := range []struct {
		name, input, path, want string
	}{
		{"index", `{"items":[{"drop":1,"keep":2},{"drop":3}]}`, "items.0.drop", `{"items":[{"keep":2},{"drop":3}]}`},
		{"wildcard", `{"items":[{"drop":1},{"drop":2},"text",null,{}]}`, "items.*.drop", `{"items":[{},{},"text",null,{}]}`},
		{"nested wildcards", `{"messages":[{"content":[{"drop":1,"text":"a"},{"drop":2}]},{"content":"b"}]}`, "messages.*.content.*.drop", `{"messages":[{"content":[{"text":"a"},{}]},{"content":"b"}]}`},
		{"literal object keys", `{"items":{"*":{"0":{"drop":1,"keep":2}},"other":{"0":{"drop":3}}}}`, "items.*.0.drop", `{"items":{"*":{"0":{"keep":2}},"other":{"0":{"drop":3}}}}`},
		{"no object wildcard", `{"items":{"a":{"drop":1}}}`, "items.*.drop", `{"items":{"a":{"drop":1}}}`},
		{"keep indexed element", `{"items":[{"drop":1},{"drop":2}]}`, "items.0", `{"items":[{"drop":1},{"drop":2}]}`},
		{"keep wildcard elements", `{"items":[{"drop":1},{"drop":2}]}`, "items.*", `{"items":[{"drop":1},{"drop":2}]}`},
		{"empty array", `{"items":[]}`, "items.*.drop", `{"items":[]}`},
		{"out of range", `{"items":[{"drop":1}]}`, "items.1.drop", `{"items":[{"drop":1}]}`},
		{"negative index", `{"items":[{"drop":1}]}`, "items.-1.drop", `{"items":[{"drop":1}]}`},
		{"invalid index", `{"items":[{"drop":1}]}`, "items.last.drop", `{"items":[{"drop":1}]}`},
		{"overflow index", `{"items":[{"drop":1}]}`, "items.999999999999999999999.drop", `{"items":[{"drop":1}]}`},
		{"missing path", `{"items":[{}]}`, "items.*.missing.drop", `{"items":[{}]}`},
		{"scalar", `{"items":"text"}`, "items.*.drop", `{"items":"text"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			params := map[string]interface{}{"pre_add": true, "remove_params": []interface{}{tc.path}}
			out, err := ApplyRawCustomParams([]byte(tc.input), params, "model", true)
			if err != nil {
				t.Fatal(err)
			}
			var got, want interface{}
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(tc.want), &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("删除 %s: got=%s want=%s", tc.path, out, tc.want)
			}
			if tc.input == tc.want && !bytes.Equal(out, []byte(tc.input)) {
				t.Fatalf("无效路径改变了原始请求: %s", out)
			}
		})
	}
}

func TestArrayParameterRemovalPreservesOtherFieldsAndRoutingIdentity(t *testing.T) {
	raw := []byte(`{"model":"selected","stream":true,"stream_format":"sse","prompt_cache_key":"partition","opaque": { "future":1e+3 },"messages":[{"content":[{"text":"hello","cache_control":{"type":"ephemeral"},"future":{"integer":900719925474099312345,"union":[null,"future"]}}]}]}`)
	params := map[string]interface{}{
		"pre_add":       true,
		"remove_params": []interface{}{"*", "*.model", "*.stream", "*.stream_format", "*.prompt_cache_key", "model", "stream", "stream_format", "messages.*.content.*.cache_control"},
	}
	out, err := ApplyRawCustomParams(raw, params, "selected", true)
	if err != nil {
		t.Fatal(err)
	}
	for _, kept := range []string{`"model":"selected"`, `"stream":true`, `"stream_format":"sse"`, `"prompt_cache_key":"partition"`, `{ "future":1e+3 }`, `"integer":900719925474099312345`, `"union":[null,"future"]`, `"text":"hello"`} {
		if !bytes.Contains(out, []byte(kept)) {
			t.Fatalf("删除数组字段丢失 %s: %s", kept, out)
		}
	}
	if bytes.Contains(out, []byte("cache_control")) {
		t.Fatalf("缓存声明未移除: %s", out)
	}
}
