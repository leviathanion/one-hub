package config

const (
	UsageExtraCache                    = "cached_tokens"               // OpenAI/兼容协议 cached_tokens 证据
	UsageExtraCacheReadInputTokens     = "cache_read_input_tokens"     // Claude cache_read_input_tokens 证据
	UsageExtraCacheWrite               = "cache_write_tokens"          // OpenAI cache_write_tokens 证据
	UsageExtraCacheCreationInputTokens = "cache_creation_input_tokens" // Claude cache_creation_input_tokens 总额
	UsageExtraEphemeral5mInputTokens   = "ephemeral_5m_input_tokens"   // Claude cache_creation.ephemeral_5m_input_tokens
	UsageExtraEphemeral1hInputTokens   = "ephemeral_1h_input_tokens"   // Claude cache_creation.ephemeral_1h_input_tokens
	UsageExtraToolUsePrompt            = "tool_use_prompt_tokens"

	UsageExtraInputAudio              = "input_audio_tokens"        // 输入音频
	UsageExtraOutputAudio             = "output_audio_tokens"       // 输出音频
	UsageExtraInputAudioTranscription = "input_audio_transcription" // 输入音频转写
	UsageExtraReasoning               = "reasoning_tokens"          // 推理
	UsageExtraInputTextTokens         = "input_text_tokens"         // 输入文本
	UsageExtraOutputTextTokens        = "output_text_tokens"        // 输出文本
	UsageExtraInputImageTokens        = "input_image_tokens"        // 输入图像
	UsageExtraOutputImageTokens       = "output_image_tokens"       // 输出图像
	UsageExtraInputVideoTokens        = "input_video_tokens"
	UsageExtraOutputVideoTokens       = "output_video_tokens"
)
