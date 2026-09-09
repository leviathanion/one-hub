export function createEndpointPreset(definitions) {
  return Object.fromEntries(definitions.map((definition) => [definition.id, { enabled: definition.default_enabled, upstream_url: '' }]));
}

// 接口 ID 中的点是名称的一部分，不能作为 Formik 嵌套路径解释。
export function updateEndpoint(plugin, id, patch) {
  return {
    ...plugin,
    endpoints: {
      ...plugin?.endpoints,
      [id]: { enabled: false, upstream_url: '', ...plugin?.endpoints?.[id], ...patch }
    }
  };
}

export function previewEndpoint(baseURL, setting, definition) {
  const uri = setting?.upstream_url || definition.default_path;
  if (/^https?:\/\//.test(uri.trim())) return uri.trim();
  let base = baseURL || '';
  if (!base) return '';
  if (base.endsWith('/')) base = base.slice(0, -1);
  const path = base.startsWith('https://gateway.ai.cloudflare.com') ? uri.replace(/^\/v1/, '') : uri;
  return base + path;
}
