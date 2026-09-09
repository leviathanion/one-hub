import PropTypes from 'prop-types';
import { Chip, Stack, Tooltip } from '@mui/material';
import { useTranslation } from 'react-i18next';
import { rateRuleEntries, rateRulesApplyToBillingType, formatRuleCondition } from './rateRulesState.mjs';

export default function RateRulesChips({ rateRules = {}, billingType, compact = false }) {
  const { t } = useTranslation();
  const entries = rateRuleEntries(rateRules);
  if (!entries.length) return null;
  const active = rateRulesApplyToBillingType(billingType);
  return (
    <Stack direction="row" spacing={0.5} useFlexGap flexWrap="wrap">
      {!active && <Chip size="small" label={t('pricing_edit.rateRules.inactive')} />}
      {entries.map(([group, rule]) => (
        <Tooltip
          key={rule.id}
          title={JSON.stringify({
            when: rule.when,
            multipliers: rule.multipliers,
            timezone: group === 'schedule' ? rateRules.schedule?.timezone : undefined
          })}
        >
          <Chip
            size={compact ? 'small' : 'medium'}
            variant="outlined"
            color={active ? 'info' : 'default'}
            label={`${t(`pricing_edit.rateRules.${group}`)} · ${formatRuleCondition(group, rule.when, (key, options) => t(`pricing_edit.rateRules.${key}`, options))}${rule.multipliers.all !== undefined ? ` · ${rule.multipliers.all}×` : ''}`}
          />
        </Tooltip>
      ))}
    </Stack>
  );
}
RateRulesChips.propTypes = { rateRules: PropTypes.object, billingType: PropTypes.string, compact: PropTypes.bool };
