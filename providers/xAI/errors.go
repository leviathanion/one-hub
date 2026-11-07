package xAI

import (
	"bytes"
	"encoding/json"
	"net/http"

	"one-api/types"
)

func RequestErrorHandle(resp *http.Response) *types.OpenAIError {
	var envelope struct {
		Error json.RawMessage `json:"error"`
		Code  any             `json:"code"`
	}
	decoder := json.NewDecoder(resp.Body)
	decoder.UseNumber()
	if decoder.Decode(&envelope) != nil {
		return nil
	}

	// xAI 原生错误使用 error 字符串；兼容端点也可能返回 OpenAI error 对象。
	var message string
	if json.Unmarshal(envelope.Error, &message) == nil && message != "" {
		return &types.OpenAIError{Message: message, Code: envelope.Code}
	}
	var detail struct {
		Message string `json:"message"`
		Code    any    `json:"code"`
		Type    string `json:"type"`
		Param   string `json:"param"`
	}
	decoder = json.NewDecoder(bytes.NewReader(envelope.Error))
	decoder.UseNumber()
	if decoder.Decode(&detail) != nil || detail.Message == "" {
		return nil
	}
	return &types.OpenAIError{
		Message: detail.Message,
		Code:    detail.Code,
		Type:    detail.Type,
		Param:   detail.Param,
	}
}
