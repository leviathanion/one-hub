export const buildManualPriceRequest = (form, expectedVersion) => {
  const request = {
    expected_version: expectedVersion,
    model: form.model,
    type: form.type,
    channel_type: form.channel_type,
    input: form.input,
    output: form.output,
    locked: Boolean(form.locked),
    extra_ratios: form.extra_ratios || {}
  };
  if (form.rate_rules !== undefined) {
    request.rate_rules = form.rate_rules;
  }
  return request;
};
