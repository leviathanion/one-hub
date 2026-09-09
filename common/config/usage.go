package config

const (
	UsageExtraCache              = "cached_tokens"       // provider cached_tokens 证据
	UsageExtraCachedRead         = "cached_read_tokens"  // provider cached_read_tokens 证据
	UsageExtraCacheWrite         = "cache_write_tokens"  // provider cache_write_tokens 证据
	UsageExtraCachedWrite        = "cached_write_tokens" // provider cached_write_tokens 证据
	UsageExtraClaudeCacheWrite5m = "claude_cache_write_5m_tokens"
	UsageExtraClaudeCacheWrite1h = "claude_cache_write_1h_tokens"
	UsageExtraToolUsePrompt      = "tool_use_prompt_tokens"

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
