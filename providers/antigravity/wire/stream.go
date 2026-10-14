package wire

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrTerminalMissing 表示 EOF 没有伴随完整的上游操作终态。
var ErrTerminalMissing = errors.New("Antigravity 上游流缺少完整终态")

// UpstreamError 保留上游错误原文，由 provider 在公开交付前观察并脱敏。
type UpstreamError struct{ Raw json.RawMessage }

func (e *UpstreamError) Error() string { return string(e.Raw) }

// UnwrapResponse 只移除 Cloud Code envelope，不重新编码 Gemini 内层。
func UnwrapResponse(raw []byte) (json.RawMessage, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope == nil {
		return nil, fmt.Errorf("Antigravity 响应不是 JSON object")
	}
	if value := envelope["error"]; hasJSONValue(value) {
		return nil, &UpstreamError{Raw: append(json.RawMessage(nil), raw...)}
	}
	inner, exists := envelope["response"]
	if !exists {
		return nil, fmt.Errorf("Antigravity 响应缺少 response envelope")
	}
	var response map[string]json.RawMessage
	if err := json.Unmarshal(inner, &response); err != nil || response == nil {
		return nil, fmt.Errorf("Antigravity response 不是 JSON object")
	}
	if value := response["error"]; hasJSONValue(value) {
		return nil, &UpstreamError{Raw: append(json.RawMessage(nil), inner...)}
	}
	return append(json.RawMessage(nil), inner...), nil
}

func hasJSONValue(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) != 0 && !bytes.Equal(trimmed, []byte("null"))
}

// NativeDecoder 只观察终态、usage 和模型证据；原生响应的未来字段不进入 typed 投影。
// Push 消费完整 SSE data，HTTP/SSE framing 和传输失败仍由 requester 负责。
type NativeDecoder struct {
	expected             int
	candidates           map[int]bool
	blocked              bool
	unobservedCandidates bool
	failed               error
	usage                json.RawMessage
	responseModel        string
}

func NewNativeDecoder(expectedCandidates int) *NativeDecoder {
	if expectedCandidates < 1 {
		expectedCandidates = 1
	}
	return &NativeDecoder{expected: expectedCandidates, candidates: make(map[int]bool)}
}

func (d *NativeDecoder) Push(data []byte) (json.RawMessage, error) {
	if d.failed != nil {
		return nil, d.failed
	}
	d.responseModel = ""
	inner, err := UnwrapResponse(data)
	if err != nil {
		// 错误帧可能同时报告最终用量；先保留原始证据再返回错误。
		var envelope map[string]json.RawMessage
		if json.Unmarshal(data, &envelope) == nil {
			if nested := envelope["response"]; hasJSONValue(nested) {
				envelope = nil
				_ = json.Unmarshal(nested, &envelope)
			}
			d.observeUsage(envelope)
		}
		d.failed = err
		return nil, err
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(inner, &fields)
	d.observeUsage(fields)
	var feedback struct {
		BlockReason string `json:"blockReason"`
	}
	if json.Unmarshal(fields["promptFeedback"], &feedback) == nil && feedback.BlockReason != "" {
		d.blocked = true
	}
	if value := fields["candidates"]; hasJSONValue(value) {
		var candidates []json.RawMessage
		if err = json.Unmarshal(value, &candidates); err != nil {
			d.unobservedCandidates = true
			return inner, nil
		}
		for position, rawCandidate := range candidates {
			var candidate map[string]json.RawMessage
			if json.Unmarshal(rawCandidate, &candidate) != nil || candidate == nil {
				d.unobservedCandidates = true
				continue
			}
			index := position
			if rawIndex, exists := candidate["index"]; exists {
				if !hasJSONValue(rawIndex) || json.Unmarshal(rawIndex, &index) != nil || index < 0 {
					d.unobservedCandidates = true
					continue
				}
			}
			var reason string
			if value, exists := candidate["finishReason"]; exists && json.Unmarshal(value, &reason) != nil {
				d.unobservedCandidates = true
				continue
			}
			if _, exists := d.candidates[index]; !exists && (len(d.candidates)+1)*64 > MaxRetainedBytes {
				return nil, d.fail(fmt.Errorf("Antigravity candidate 状态超过转换容量"))
			}
			d.candidates[index] = d.candidates[index] || reason != ""
		}
	}
	return inner, nil
}

func (d *NativeDecoder) observeUsage(fields map[string]json.RawMessage) {
	_ = json.Unmarshal(fields["modelVersion"], &d.responseModel)
	raw := fields["usageMetadata"]
	if hasJSONValue(raw) {
		d.usage = append(d.usage[:0], raw...)
	}
}

func (d *NativeDecoder) fail(err error) error { d.failed = err; return err }

func (d *NativeDecoder) UsageRaw() json.RawMessage { return append(json.RawMessage(nil), d.usage...) }

// ResponseModel 返回当前帧证据，跨帧保留及冲突由共享归属规则处理。
func (d *NativeDecoder) ResponseModel() string { return d.responseModel }

func (d *NativeDecoder) Finish() error {
	if d.failed != nil {
		return d.failed
	}
	// 未知联合类型照常交付，但不能据已识别候选推断整请求已完成。
	if d.unobservedCandidates {
		return ErrTerminalMissing
	}
	if d.blocked && len(d.candidates) == 0 {
		return nil
	}
	if len(d.candidates) < d.expected {
		return ErrTerminalMissing
	}
	for _, finished := range d.candidates {
		if !finished {
			return ErrTerminalMissing
		}
	}
	return nil
}
