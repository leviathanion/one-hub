package types

import "encoding/json"

type CompletionRequest struct {
	Model            string         `json:"model" binding:"required"`
	Prompt           any            `json:"prompt" binding:"required"`
	Suffix           string         `json:"suffix,omitempty"`
	MaxTokens        int            `json:"max_tokens,omitempty"`
	Temperature      float32        `json:"temperature,omitempty"`
	TopP             float32        `json:"top_p,omitempty"`
	N                int            `json:"n,omitempty"`
	Stream           bool           `json:"stream,omitempty"`
	StreamOptions    *StreamOptions `json:"stream_options,omitempty"`
	LogProbs         int            `json:"logprobs,omitempty"`
	Echo             bool           `json:"echo,omitempty"`
	Stop             []string       `json:"stop,omitempty"`
	PresencePenalty  float32        `json:"presence_penalty,omitempty"`
	FrequencyPenalty float32        `json:"frequency_penalty,omitempty"`
	BestOf           int            `json:"best_of,omitempty"`
	LogitBias        any            `json:"logit_bias,omitempty"`
	User             string         `json:"user,omitempty"`
}

type CompletionChoice struct {
	Text         string `json:"text"`
	Index        int    `json:"index"`
	FinishReason string `json:"finish_reason"`
	LogProbs     any    `json:"logprobs,omitempty"`
}

type CompletionResponse struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Created any                `json:"created"`
	Model   string             `json:"model"`
	Choices []CompletionChoice `json:"choices"`
	Usage   *Usage             `json:"usage,omitempty"`

	rawProviderJSON        []byte
	replayProviderRawJSON  bool
	captureProviderRawJSON bool
}

func (r CompletionResponse) MarshalJSON() ([]byte, error) {
	type responseAlias CompletionResponse
	return json.Marshal(struct {
		responseAlias
		Usage *chatCompletionUsage `json:"usage,omitempty"`
	}{
		responseAlias: responseAlias(r),
		Usage:         projectChatCompletionUsage(r.Usage),
	})
}

func (r *CompletionResponse) SetProviderRawJSON(raw []byte) {
	if r != nil {
		r.rawProviderJSON = append(r.rawProviderJSON[:0], raw...)
	}
}

func (r *CompletionResponse) ProviderRawJSON() []byte {
	if r == nil {
		return nil
	}
	return append([]byte(nil), r.rawProviderJSON...)
}

func (r *CompletionResponse) EnableProviderRawJSONCapture() {
	if r != nil {
		r.captureProviderRawJSON = true
	}
}

func (r *CompletionResponse) CaptureProviderRawJSON() bool {
	return r != nil && r.captureProviderRawJSON
}

func (r *CompletionResponse) EnableProviderRawJSONReplay() {
	if r != nil {
		r.replayProviderRawJSON = true
	}
}

func (r *CompletionResponse) ReplayProviderRawJSON() []byte {
	if r == nil || !r.replayProviderRawJSON {
		return nil
	}
	return r.ProviderRawJSON()
}
