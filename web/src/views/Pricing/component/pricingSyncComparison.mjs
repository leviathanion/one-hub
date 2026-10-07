import { stablePricingJson } from './pricingComparison.mjs';
import { getExtraRatioLabel } from './extraRatiosState.mjs';
import { RATE_RULE_GROUPS, getRateGroupRules, formatRuleCondition } from './rateRulesState.mjs';

const equal = (a, b) => stablePricingJson(a) === stablePricingJson(b);

const describeRules = (rules, group, t) => {
  const tr = (key, options) => t(`pricing_edit.rateRules.${key}`, options);
  const lines = getRateGroupRules(rules, group).map((rule) => {
    const factors = Object.entries(rule.multipliers || {}).flatMap(([key, value]) =>
      key === 'extra_multipliers'
        ? Object.entries(value).map(([meter, factor]) => `${getExtraRatioLabel(t, meter)} ${factor}×`)
        : [`${t(`pricingSync.factor.${key}`)} ${value}×`]
    );
    return `${rule.id} · ${formatRuleCondition(group, rule.when, tr)} · ${factors.join(', ') || t('pricingSync.defaultFactor')}`;
  });
  if (group === 'schedule' && rules?.schedule?.timezone) lines.unshift(`${tr('timezone')}: ${rules.schedule.timezone}`);
  return lines.join('\n');
};

// Render server-approved policy changes; do not recompute the change plan here.
export const pricingSyncRows = (before, after, t) => {
  const rows = [];
  const add = (key, label, oldValue, newValue) => rows.push({ key, label, before: oldValue, after: newValue });
  for (const key of ['input', 'output', 'type', 'locked']) {
    if (before && after && equal(before[key], after[key])) continue;
    const value = (price) => {
      if (!price || price[key] == null) return '';
      if (key === 'type') return t(`modelpricePage.${price[key]}`);
      if (key === 'locked') return t(price[key] ? 'pricing_edit.locked' : 'pricing_edit.unlocked');
      return String(price[key]);
    };
    if (value(before) || value(after)) add(key, t(`pricingSync.field.${key}`), value(before), value(after));
  }
  const oldExtras = before?.extra_ratios || {};
  const newExtras = after?.extra_ratios || {};
  for (const key of [...new Set([...Object.keys(oldExtras), ...Object.keys(newExtras)])].sort()) {
    if (equal(oldExtras[key], newExtras[key])) continue;
    add(`extra:${key}`, getExtraRatioLabel(t, key), oldExtras[key]?.toString() ?? '', newExtras[key]?.toString() ?? '');
  }
  if (!Object.keys(oldExtras).length && !Object.keys(newExtras).length && !equal(before?.extra_ratios, after?.extra_ratios)) {
    add(
      'extras',
      t('CheckUpdatesTable.extraRatiosChanges'),
      before?.extra_ratios ? t('pricingSync.emptyExtras') : '',
      after?.extra_ratios ? t('pricingSync.emptyExtras') : ''
    );
  }
  const ruleRowsStart = rows.length;
  for (const group of RATE_RULE_GROUPS) {
    const oldRules = before?.rate_rules?.[group];
    const newRules = after?.rate_rules?.[group];
    if (equal(oldRules, newRules)) continue;
    add(
      `rule:${group}`,
      t(`pricing_edit.rateRules.${group}`),
      describeRules(before?.rate_rules, group, t),
      describeRules(after?.rate_rules, group, t)
    );
  }
  if (rows.length === ruleRowsStart && !equal(before?.rate_rules, after?.rate_rules)) {
    add(
      'rules',
      t('pricingSync.field.rules'),
      before?.rate_rules ? t('pricingSync.emptyRules') : '',
      after?.rate_rules ? t('pricingSync.emptyRules') : ''
    );
  }
  return rows;
};
