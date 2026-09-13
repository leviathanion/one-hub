export const EMPTY_MODELS = [];

export const getModelId = (model) => (typeof model === 'string' ? model : model.id);

export const asModels = (models) => (Array.isArray(models) ? models : EMPTY_MODELS);

// 模型 ID 是选择身份；目录、手动输入和渠道拉取的对象引用可以不同。
export function normalizeModelSelection(models, optionsById, customGroup) {
  const seen = new Set();
  const result = [];
  for (const model of models) {
    const id = getModelId(model);
    if (id === '' || seen.has(id)) continue;
    seen.add(id);
    result.push(optionsById.get(id) || (typeof model === 'string' ? { id, group: customGroup } : model));
  }
  return result;
}

export const MODEL_ROW_HEIGHT = 48;
export const MODEL_VISIBLE_ROWS = 8;
export const MODEL_OVERSCAN = 3;

export function modelWindow(scrollTop, rowCount) {
  const count = MODEL_VISIBLE_ROWS + MODEL_OVERSCAN * 2;
  const start = Math.min(Math.max(0, Math.floor(scrollTop / MODEL_ROW_HEIGHT) - MODEL_OVERSCAN), Math.max(0, rowCount - count));
  return { start, end: Math.min(rowCount, start + count) };
}

// 与此字段使用的 MUI v5 导航规则一致；只准备目标 DOM，选择和高亮仍交给 MUI。
export function modelNavigationTarget(key, current, count, inputValue) {
  if (count === 0) return -1;
  const last = count - 1;
  switch (key) {
    case 'ArrowDown':
      return current >= last ? 0 : current + 1;
    case 'ArrowUp':
      return current <= 0 ? last : current - 1;
    case 'PageDown':
      return Math.min(last, current + 5);
    case 'PageUp':
      return Math.max(0, current - 5);
    case 'Home':
      return inputValue === '' ? 0 : -1;
    case 'End':
      return inputValue === '' ? last : -1;
    default:
      return -1;
  }
}
