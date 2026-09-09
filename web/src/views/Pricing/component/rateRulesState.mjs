export const RATE_RULE_GROUPS = ['service_tier', 'speed', 'long_context', 'schedule'];
export const RATE_EXTRA_KEYS = [
  'cached_tokens',
  'cache_write_tokens',
  'cached_write_tokens',
  'cached_read_tokens',
  'claude_cache_write_5m_tokens',
  'claude_cache_write_1h_tokens',
  'tool_use_prompt_tokens',
  'input_audio_tokens',
  'output_audio_tokens',
  'reasoning_tokens',
  'input_text_tokens',
  'output_text_tokens',
  'input_image_tokens',
  'output_image_tokens',
  'input_video_tokens',
  'output_video_tokens'
];
export const rateRulesApplyToBillingType = (type) => type === 'tokens';
export const getRateGroupRules = (rules, group) => (group === 'schedule' ? rules?.schedule?.rules || [] : rules?.[group] || []);
export const rateRuleEntries = (rules) => RATE_RULE_GROUPS.flatMap((group) => getRateGroupRules(rules, group).map((rule) => [group, rule]));
export function formatRuleCondition(group, when = {}, tr) {
  if (group === 'service_tier') return (when.service_tier || []).join(' / ') || tr('selectValues');
  if (group === 'speed') return (when.speed || []).join(' / ') || tr('selectValues');
  if (group === 'long_context') {
    const range = when.input_tokens || {};
    const bounds = [range.gt !== undefined ? `> ${range.gt}` : '', range.lte !== undefined ? `≤ ${range.lte}` : '']
      .filter(Boolean)
      .join(' · ');
    return `${tr('inputRange', { range: bounds || '—' })}${when.speed?.length ? ` · ${when.speed.join(' / ')}` : ''}`;
  }
  const days = when.weekdays?.length ? when.weekdays.map((day) => tr(`day${day}`)).join(' / ') : tr('everyDay');
  return `${days} · ${when.time ? `${when.time.start || '—'}–${when.time.end || '—'}` : tr('allDay')}`;
}
export function rateConditionsOverlap(group, a, b) {
  const intersects = (left, right) => !left?.length || !right?.length || left.some((value) => right.includes(value));
  if (group === 'service_tier') return intersects(a.service_tier, b.service_tier);
  if (group === 'speed') return intersects(a.speed, b.speed);
  if (group === 'long_context')
    return (
      intersects(a.speed, b.speed) &&
      Math.max(Number(a.input_tokens?.gt ?? -1), Number(b.input_tokens?.gt ?? -1)) <
        Math.min(Number(a.input_tokens?.lte ?? Number.MAX_SAFE_INTEGER), Number(b.input_tokens?.lte ?? Number.MAX_SAFE_INTEGER))
    );
  return false;
}
const number = (value) => value !== '' && value !== null && value !== undefined && Number.isFinite(Number(value)) && Number(value) >= 0;
const minute = (value) => /^([01]\d|2[0-3]):[0-5]\d$/.test(value || '');

export function validateRateRules(rules = {}) {
  const errors = {};
  const fail = (path) => {
    errors[path] = 'invalid';
  };
  if (!rules || typeof rules !== 'object' || Array.isArray(rules)) return { rules: 'invalid' };
  if (Object.keys(rules).some((key) => !['version', ...RATE_RULE_GROUPS].includes(key))) fail('rules');
  if (Object.keys(rules).length && rules.version !== 2) fail('version');
  const timezone = rules.schedule?.timezone;
  if (
    rules.schedule !== undefined &&
    (!rules.schedule ||
      Array.isArray(rules.schedule) ||
      typeof rules.schedule !== 'object' ||
      Object.keys(rules.schedule).some((key) => !['timezone', 'rules'].includes(key)))
  )
    fail('schedule');
  if (timezone) {
    try {
      new Intl.DateTimeFormat('en', { timeZone: timezone }).format();
    } catch {
      fail('schedule.timezone');
    }
    if (timezone === 'Local') fail('schedule.timezone');
  }
  const ids = new Set();
  let count = 0;
  for (const group of RATE_RULE_GROUPS) {
    const rows = getRateGroupRules(rules, group);
    if (!Array.isArray(rows)) {
      fail(group);
      continue;
    }
    for (const [index, rule] of rows.entries()) {
      const path = `${group}.${index}`;
      if (++count > 64 || !rule || typeof rule !== 'object') {
        fail(path);
        continue;
      }
      if (!rule.id?.trim() || rule.id !== rule.id.trim() || rule.id.length > 100 || ids.has(rule.id)) fail(`${path}.id`);
      ids.add(rule.id);
      if (Object.keys(rule).some((key) => !['id', 'when', 'multipliers'].includes(key))) fail(path);
      const when = rule.when || {};
      const allowed = {
        service_tier: ['service_tier'],
        speed: ['speed'],
        long_context: ['input_tokens', 'speed'],
        schedule: ['weekdays', 'time']
      }[group];
      if (Object.keys(when).some((key) => !allowed.includes(key))) fail(`${path}.when`);
      if (
        (group === 'service_tier' && !when.service_tier?.length) ||
        (group === 'speed' && !when.speed?.length) ||
        (group === 'long_context' && !when.input_tokens)
      )
        fail(`${path}.when`);
      if (group !== 'schedule' && rows.slice(0, index).some((earlier) => rateConditionsOverlap(group, earlier.when || {}, when)))
        errors[`${path}.when`] = 'overlap';
      for (const key of ['service_tier', 'speed']) {
        if (
          when[key] !== undefined &&
          (!Array.isArray(when[key]) ||
            !when[key].length ||
            when[key].length > 32 ||
            when[key].some((value) => typeof value !== 'string' || !value.trim()) ||
            new Set(when[key]).size !== when[key].length)
        )
          fail(`${path}.when.${key}`);
      }
      if (!Object.keys(when).length && index < rows.length - 1) fail(`${path}.when`);
      if (when.input_tokens) {
        const range = when.input_tokens;
        if (
          !Object.keys(range).length ||
          Object.keys(range).some((key) => !['gt', 'lte'].includes(key) || !number(range[key]) || !Number.isSafeInteger(Number(range[key])))
        )
          fail(`${path}.when.input_tokens`);
        if (range.gt !== undefined && range.lte !== undefined && Number(range.gt) >= Number(range.lte)) fail(`${path}.when.input_tokens`);
      }
      if (
        when.weekdays &&
        (!Array.isArray(when.weekdays) ||
          !when.weekdays.length ||
          when.weekdays.some((value) => !Number.isInteger(value) || value < 1 || value > 7) ||
          new Set(when.weekdays).size !== when.weekdays.length)
      )
        fail(`${path}.when.weekdays`);
      if (when.time && (!minute(when.time.start) || !minute(when.time.end) || when.time.start === when.time.end)) fail(`${path}.when.time`);
      if ((when.time || when.weekdays) && !timezone) fail('schedule.timezone');
      const multipliers = rule.multipliers || {};
      if (Object.keys(multipliers).some((key) => !['all', 'input', 'output', 'extra_multipliers'].includes(key)))
        fail(`${path}.multipliers`);
      for (const key of ['all', 'input', 'output'])
        if (multipliers[key] !== undefined && !number(multipliers[key])) fail(`${path}.multipliers.${key}`);
      for (const [key, value] of Object.entries(multipliers.extra_multipliers || {}))
        if (!RATE_EXTRA_KEYS.includes(key) || !number(value)) fail(`${path}.multipliers.${key}`);
    }
  }
  return errors;
}

export function normalizeRateRules(rules = {}) {
  if (Object.keys(validateRateRules(rules)).length) throw new TypeError('conditional price rules contain invalid values');
  if (!RATE_RULE_GROUPS.some((key) => getRateGroupRules(rules, key).length)) return undefined;
  const result = structuredClone(rules);
  result.version = 2;
  for (const [, rule] of rateRuleEntries(result)) {
    for (const key of ['all', 'input', 'output'])
      if (rule.multipliers[key] !== undefined) rule.multipliers[key] = Number(rule.multipliers[key]);
    for (const key of Object.keys(rule.multipliers.extra_multipliers || {}))
      rule.multipliers.extra_multipliers[key] = Number(rule.multipliers.extra_multipliers[key]);
    for (const key of Object.keys(rule.when.input_tokens || {})) rule.when.input_tokens[key] = Number(rule.when.input_tokens[key]);
  }
  return result;
}

export const rateRulesPayload = (rules, explicit) => normalizeRateRules(rules) ?? (explicit ? {} : undefined);
