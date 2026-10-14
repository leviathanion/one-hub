package wire

import (
	"encoding/json"
	"math"

	"one-api/providers/gemini"
)

// usageFields 只生成真实报告或可由上游证据证明的字段；原始 usage 独立供计费 reducer 使用。
func usageFields(raw json.RawMessage, dialect Dialect) map[string]any {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return nil
	}
	read := func(name string) (int64, bool) {
		var n int64
		v, exists := fields[name]
		if !exists || string(v) == "null" || json.Unmarshal(v, &n) != nil || n < 0 {
			return 0, false
		}
		return n, true
	}
	input, inOK := read("promptTokenCount")
	tool, toolOK := read("toolUsePromptTokenCount")
	output, outOK := read("candidatesTokenCount")
	if _, exists := fields["candidatesTokenCount"]; !exists {
		var metadata gemini.GeminiUsageMetadata
		outOK = json.Unmarshal(raw, &metadata) == nil && metadata.CanProveZeroCandidates()
	}
	thought, thoughtOK := read("thoughtsTokenCount")
	cache, cacheOK := read("cachedContentTokenCount")
	if _, exists := fields["toolUsePromptTokenCount"]; exists && !toolOK {
		inOK = false
	}
	if _, exists := fields["thoughtsTokenCount"]; exists && !thoughtOK {
		outOK = false
	}
	if toolOK && inOK {
		if tool > math.MaxInt64-input {
			inOK = false
		} else {
			input += tool
		}
	}
	if thoughtOK && outOK {
		if thought > math.MaxInt64-output {
			outOK = false
		} else {
			output += thought
		}
	}
	result := make(map[string]any)
	if dialect == DialectClaude {
		if _, exists := fields["cachedContentTokenCount"]; exists && !cacheOK {
			inOK = false
		}
		if inOK && (!cacheOK || cache <= input) {
			if cacheOK {
				input -= cache
			}
			result["input_tokens"] = input
		}
		if outOK {
			result["output_tokens"] = output
		}
		if cacheOK {
			result["cache_read_input_tokens"] = cache
		}
	} else {
		inName, outName := "prompt_tokens", "completion_tokens"
		if dialect == DialectResponses {
			inName, outName = "input_tokens", "output_tokens"
		}
		if inOK {
			result[inName] = input
		}
		if outOK {
			result[outName] = output
		}
		if total, ok := read("totalTokenCount"); ok {
			result["total_tokens"] = total
		}
		if cacheOK {
			result[inName+"_details"] = map[string]any{"cached_tokens": cache}
		}
		if thoughtOK {
			result[outName+"_details"] = map[string]any{"reasoning_tokens": thought}
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}
