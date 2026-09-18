const extraRatiosConfig = [
  {
    key: 'cached_tokens',
    isPrompt: true
  },
  {
    key: 'cache_write_tokens',
    isPrompt: true
  },
  {
    key: 'cache_creation_input_tokens',
    isPrompt: true
  },
  {
    key: 'ephemeral_5m_input_tokens',
    isPrompt: true
  },
  {
    key: 'ephemeral_1h_input_tokens',
    isPrompt: true
  },
  {
    key: 'cache_read_input_tokens',
    isPrompt: true
  },
  {
    key: 'input_audio_tokens',
    isPrompt: true
  },
  {
    key: 'input_audio_transcription',
    isPrompt: true
  },
  {
    key: 'output_audio_tokens',
    isPrompt: false
  },
  {
    key: 'reasoning_tokens',
    isPrompt: false
  },
  {
    key: 'input_text_tokens',
    isPrompt: true
  },
  {
    key: 'output_text_tokens',
    isPrompt: false
  },
  {
    key: 'input_image_tokens',
    isPrompt: true
  },
  {
    key: 'output_image_tokens',
    isPrompt: false
  }
];

export { extraRatiosConfig };
