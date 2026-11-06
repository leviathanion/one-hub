export const MAX_OWNERSHIP_BATCH = 500;
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
export function buildOwnershipBatch({ selected, method, target, mode, suggestions, labOwners, owners }) {
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
    const lab = suggestion?.status === 'matched' && suggestion.labs?.length === 1 ? suggestion.labs[0] : null;
    const owner = lab != null && Object.hasOwn(labOwners, lab) ? labOwners[lab] : null;
    if (!owner || !Object.hasOwn(owners, owner)) unresolved.push(model);
    else items.push({ model, owned_by_id: Number(owner) });
  }
  if (unresolved.length) {
    return {
      items: [],
      error: `${unresolved.length} 个模型尚未确定归属：${unresolved.slice(0, 5).join('、')}${unresolved.length > 5 ? '等' : ''}。请补全来源映射，或取消选择未匹配、冲突的模型。`
    };
  }
  return { items };
}
