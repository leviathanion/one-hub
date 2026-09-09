export const DEFAULT_AZURE_API_VERSION = '2024-05-01-preview';

const OPENAI_COMPATIBLE_OTHER_FIELDS = new Set(['responses_ws_native', 'responses_ws_self_hosted', 'self_hosted', 'extra', 'vendor_extra']);

const parseOtherObject = (raw) => {
  const trimmed = String(raw ?? '').trim();
  if (trimmed === '') {
    return {};
  }
  const parsed = JSON.parse(trimmed);
  if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) {
    throw new Error('other must be a JSON object');
  }
  return parsed;
};

export const isSelfHostedResponsesWSEnabled = (rawOther) => {
  try {
    const other = parseOtherObject(rawOther);
    return other.responses_ws_self_hosted === true;
  } catch {
    return false;
  }
};

export const normalizeChannelOtherForRequest = (sourceValues) => {
  const values = { ...sourceValues };
  if (Number(values.type) === 3 && String(values.other ?? '').trim() === '') {
    values.other = JSON.stringify({ api_version: DEFAULT_AZURE_API_VERSION });
  }
  if (String(values.other ?? '').trim() !== '') {
    parseOtherObject(values.other);
  }
  // Drop obsolete form mirrors without changing the raw JSON, which is the
  // sole owner of Responses WS channel settings.
  delete values.responses_ws_native;
  delete values.responses_ws_self_hosted;
  return values;
};

export const normalizeOpenAICompatibleOtherForRequest = (sourceValues, options = {}) => {
  const values = { ...sourceValues, type: 1 };
  delete values.responses_ws_native;
  delete values.responses_ws_self_hosted;
  const raw = String(values.other ?? '').trim();
  if (raw === '') {
    return values;
  }

  try {
    const parsed = JSON.parse(raw);
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) {
      throw new Error('other must be a JSON object');
    }

    const compatible = {};
    for (const key of Object.keys(parsed)) {
      if (!OPENAI_COMPATIBLE_OTHER_FIELDS.has(key)) {
        // Used only by the model selector's temporary OpenAI-compatible fetch request.
        // It must not persist the stripped provider-specific fields back to the channel.
        if (options.dropUnsupportedFields) {
          continue;
        }
        throw new Error(`other.${key} is not supported for OpenAI-compatible model fetch`);
      }
      compatible[key] = parsed[key];
    }
    values.other = Object.keys(compatible).length > 0 ? JSON.stringify(compatible) : '';
  } catch (error) {
    if (error?.message?.startsWith('other.')) {
      throw error;
    }
    throw new Error('other must be a valid JSON object');
  }
  return values;
};

export const PreCostType = [
  { value: 1, label: '正常计费' },
  { value: 2, label: '不计算图片' },
  { value: 3, label: '全部不计算' }
];
