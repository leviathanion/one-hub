import { formatRuleCondition } from '../../Pricing/component/rateRulesState.mjs';
import { Stack, Typography } from '@mui/material';
import PropTypes from 'prop-types';
import { useTranslation } from 'react-i18next';
import { formatBillingNumber, getGroupRatio, getTokenBillingDetails } from './quotaDetail';

export default function BillingRuleDetails({ item }) {
  const { t, i18n } = useTranslation();
  const text = (key, values) => t('logPage.quotaDetail.' + key, values);
  const number = (value) =>
    typeof value === 'number' && Number.isFinite(value) ? formatBillingNumber(value, i18n.language) : text('notRecorded');
  const metadata = item.metadata || {};
  const details = getTokenBillingDetails(metadata);
  const groupRatio = getGroupRatio(metadata);
  const reasons = [];
  let note;

  if (metadata.price_type !== 'times') {
    if (!details) {
      note = text('missingRuleDetails');
    } else if (details.status !== 'priceable') {
      const statuses = { conflicting_evidence: 'conflictingUsage', missing_evidence: 'missingUsage', not_applicable: 'noTokenCharge' };
      note = text(statuses[details.status] || 'noTokenCharge');
    } else {
      for (const rule of details.rules) {
        const extra = Object.entries(rule.extra_multipliers || {});
        if (rule.input_multiplier === 1 && rule.output_multiplier === 1 && extra.length === 0) continue;
        reasons.push(
          t('pricing_edit.rateRules.ruleReason', {
            name: formatRuleCondition(rule.kind, rule.when, (key, options) => t(`pricing_edit.rateRules.${key}`, options)),
            input: number(rule.input_multiplier),
            output: number(rule.output_multiplier)
          })
        );
        if (rule.when?.input_tokens?.gt !== undefined)
          reasons.push(
            text('longContextReason', {
              countText: number(item.prompt_tokens),
              threshold: number(rule.when.input_tokens.gt),
              input: number(rule.input_multiplier),
              output: number(rule.output_multiplier)
            })
          );
        if (extra.length)
          reasons.push(
            t('pricing_edit.rateRules.extraReason', {
              items: extra.map(([key, value]) => `${t(`modelpricePage.${key}`, { defaultValue: key })} ×${number(value)}`).join(', ')
            })
          );
      }
    }
  }
  if (groupRatio !== 1) {
    reasons.push(text(groupRatio < 1 ? 'groupDiscountReason' : 'groupMarkupReason', { percent: number(Math.abs(1 - groupRatio) * 100) }));
  }
  if (!reasons.length && !note) return null;

  return (
    <Stack spacing={0.75}>
      {reasons.length > 0 && <Typography variant="subtitle2">{text('priceReasons')}</Typography>}
      {reasons.map((reason, index) => (
        <Typography key={index} variant="body2">
          {reason}
        </Typography>
      ))}
      {note && (
        <Typography variant="body2" color="text.secondary">
          {note}
        </Typography>
      )}
    </Stack>
  );
}

BillingRuleDetails.propTypes = {
  item: PropTypes.shape({ metadata: PropTypes.object, prompt_tokens: PropTypes.number }).isRequired
};
