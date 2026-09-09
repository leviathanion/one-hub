package types

import (
	"encoding/json"
	"mime/multipart"
	"net/http"
)

type SpeechAudioRequest struct {
	Model          string          `json:"model" binding:"required"`
	Input          string          `json:"input" binding:"required"`
	Voice          json.RawMessage `json:"voice" binding:"required"`
	ResponseFormat string          `json:"response_format,omitempty"`
	Speed          float64         `json:"speed,omitempty"`
	StreamFormat   string          `json:"stream_format,omitempty"`
}

func (r SpeechAudioRequest) VoiceString() (string, bool) {
	if len(r.Voice) == 0 {
		return "", false
	}
	var voice string
	if json.Unmarshal(r.Voice, &voice) != nil || voice == "" {
		return "", false
	}
	return voice, true
}

type AudioRequest struct {
	File           *multipart.FileHeader `form:"file" binding:"required"`
	Model          string                `form:"model" binding:"required"`
	Language       string                `form:"language"`
	Prompt         string                `form:"prompt"`
	ResponseFormat string                `form:"response_format"`
	Temperature    float32               `form:"temperature"`
	Stream         bool                  `form:"stream"`
}

type AudioResponse struct {
	Task     string           `json:"task,omitempty"`
	Language string           `json:"language,omitempty"`
	Duration float64          `json:"duration,omitempty"`
	Segments any              `json:"segments,omitempty"`
	Text     string           `json:"text"`
	Words    []AudioWordsList `json:"words,omitempty"`
	Usage    *AudioUsage      `json:"usage,omitempty"`
	Model    string           `json:"model,omitempty"`
}

type AudioUsage struct {
	Type         string                  `json:"type,omitempty"`
	InputTokens  *int                    `json:"input_tokens,omitempty"`
	OutputTokens *int                    `json:"output_tokens,omitempty"`
	TotalTokens  *int                    `json:"total_tokens,omitempty"`
	Seconds      *float64                `json:"seconds,omitempty"`
	InputDetails *AudioUsageInputDetails `json:"input_token_details,omitempty"`
}

type AudioUsageInputDetails struct {
	TextTokens  *int `json:"text_tokens,omitempty"`
	AudioTokens *int `json:"audio_tokens,omitempty"`
}

type AudioWordsList struct {
	Word  string  `json:"word"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

type AudioResponseWrapper struct {
	Headers              map[string]string
	Body                 []byte
	Stream               *http.Response
	ObserveProviderEvent func([]byte)
}
