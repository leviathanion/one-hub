export function buildUserEditPatch(values, original, userId) {
  const patch = { id: Number(userId) };
  for (const key of ['username', 'display_name', 'group']) {
    if (values[key] !== original[key]) patch[key] = values[key];
  }
  if (values.password) patch.password = values.password;
  return patch;
}
