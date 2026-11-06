export const MAX_OWNERSHIP_BATCH = 5000;
export const CLEAR_OWNERSHIP = '__clear__';

export function parseOwnershipModels(text) {
  const models = [
    ...new Set(
      text
        .split(/\r?\n/)
        .map((model) => model.trim())
        .filter(Boolean)
    )
  ];
  if (models.length === 0) throw new Error('请每行输入一个精确模型名');
  if (models.some((model) => model.includes('*') || [...model].length > 100)) {
    throw new Error('模型名不能包含通配符 *，且不能超过 100 个字符');
  }
  if (models.length > MAX_OWNERSHIP_BATCH) throw new Error(`每次最多选择 ${MAX_OWNERSHIP_BATCH} 个模型`);
  return models;
}

// 只构造归属更新，不把候选中的模型说明或价格重新写回服务端。
export function buildOwnershipBatch({ selected, method, target, mode, suggestions, providerOwners, owners }) {
  const models = [...selected];
  if (!models.length) return { error: '请先选择模型', items: [] };
  if (models.length > MAX_OWNERSHIP_BATCH) return { error: `每次最多选择 ${MAX_OWNERSHIP_BATCH} 个模型`, items: [] };
  if (method === 'manual') {
    if (target === CLEAR_OWNERSHIP) {
      if (mode !== 'replace') return { error: '清空归属需要选择覆盖所选模型', items: [] };
      return { items: models.map((model) => ({ model, owned_by_id: null })) };
    }
    if (!target || !Object.hasOwn(owners, target)) return { error: '请选择有效的目标归属', items: [] };
    return { items: models.map((model) => ({ model, owned_by_id: Number(target) })) };
  }

  const unresolved = [];
  const items = [];
  for (const model of models) {
    const suggestion = Object.hasOwn(suggestions, model) ? suggestions[model] : undefined;
    const providerID = suggestion?.status === 'matched' && suggestion.providers?.length === 1 ? suggestion.providers[0].id : null;
    const owner = providerID != null && Object.hasOwn(providerOwners, providerID) ? providerOwners[providerID] : null;
    if (!owner || !Object.hasOwn(owners, owner)) unresolved.push(model);
    else items.push({ model, owned_by_id: Number(owner) });
  }
  if (!items.length) {
    return {
      items,
      unresolved,
      error: '暂无可保存的模型，请查询服务提供方，并至少选择一项本系统归属映射。'
    };
  }
  return { items, unresolved };
}
