export const modelsDevSelectionKey = (candidate) => JSON.stringify([candidate.provider, candidate.model]);

export const selectModelsDevCandidate = (selected, candidate, checked) => {
  const next = { ...selected };
  if (checked && candidate.price && !candidate.reason) return { ...selected, [candidate.model]: candidate };
  if (next[candidate.model]?.provider === candidate.provider) delete next[candidate.model];
  return next;
};

export const modelsDevSelectedSource = (selected) =>
  Object.values(selected)
    .sort((a, b) => a.model.localeCompare(b.model))
    .map((candidate) => candidate.price);

export const ordinaryPricingMode = (mode) => (mode === 'merge' ? 'overwrite' : mode);
