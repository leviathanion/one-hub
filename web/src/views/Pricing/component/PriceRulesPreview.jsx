import { useEffect, useRef, useState } from 'react';
import PropTypes from 'prop-types';
import { Accordion, AccordionSummary, AccordionDetails, Alert, Button, MenuItem, Stack, TextField, Typography } from '@mui/material';
import { useTranslation } from 'react-i18next';
import { API } from 'utils/api';
import { RATE_EXTRA_KEYS, rateRulesPayload } from './rateRulesState.mjs';
import QuotaWithDetailContent from '../../Log/component/QuotaWithDetailContent';

export default function PriceRulesPreview({ price }) {
  const { t } = useTranslation();
  const tr = (key) => t(`pricing_edit.rateRules.${key}`);
  const [facts, setFacts] = useState({ service_tier: 'default', speed: '', started_at: new Date().toISOString() });
  const [counts, setCounts] = useState({
    prompt_tokens: 1000,
    completion_tokens: 100,
    cached_read_tokens: 0,
    claude_cache_write_5m_tokens: 0,
    claude_cache_write_1h_tokens: 0
  });
  const [extra, setExtra] = useState('');
  const [result, setResult] = useState(null);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const generation = useRef(0);
  const signature = JSON.stringify([price, facts, counts]);
  useEffect(() => {
    generation.current++;
    setResult(null);
    setError('');
    setBusy(false);
  }, [signature]);
  const preview = async () => {
    const ticket = ++generation.current;
    setBusy(true);
    setError('');
    setResult(null);
    try {
      const draft = { ...price, rate_rules: rateRulesPayload(price.rate_rules, true) };
      const usage = { prompt_tokens: Number(counts.prompt_tokens), completion_tokens: Number(counts.completion_tokens) };
      const extra_tokens = Object.fromEntries(
        Object.entries(counts)
          .filter(([key]) => !Object.hasOwn(usage, key))
          .map(([key, value]) => [key, Number(value)])
      );
      const response = await API.post('/api/prices/preview', { price: draft, facts, usage, extra_tokens });
      if (ticket !== generation.current) return;
      if (!response.data.success) throw new Error(response.data.message || tr('invalid'));
      setResult({ ...usage, quota: response.data.data.decision.FinalQuota, metadata: response.data.data.metadata });
    } catch (failure) {
      if (ticket === generation.current) setError(failure.response?.data?.message || failure.message);
    } finally {
      if (ticket === generation.current) setBusy(false);
    }
  };
  return (
    <Accordion>
      <AccordionSummary expandIcon={<span aria-hidden="true">⌄</span>}>{tr('preview')}</AccordionSummary>
      <AccordionDetails>
        <Stack spacing={1.5}>
          <Alert severity="info">{tr('previewHelp')}</Alert>
          {Object.entries(facts).map(([key, value]) => (
            <TextField key={key} label={tr(key)} value={value} onChange={(event) => setFacts({ ...facts, [key]: event.target.value })} />
          ))}
          {Object.entries(counts).map(([key, value]) => (
            <TextField
              key={key}
              label={RATE_EXTRA_KEYS.includes(key) ? `${tr(`meters.${key}`)} (Token)` : tr(key)}
              type="number"
              inputProps={{ min: 0, step: 1 }}
              value={value}
              onChange={(event) => setCounts({ ...counts, [key]: event.target.value })}
            />
          ))}
          <Stack direction="row" spacing={1}>
            <TextField fullWidth select label={tr('extra')} value={extra} onChange={(event) => setExtra(event.target.value)}>
              <MenuItem value="">{tr('selectExtra')}</MenuItem>
              {RATE_EXTRA_KEYS.filter((key) => counts[key] === undefined).map((key) => (
                <MenuItem key={key} value={key}>
                  {t(`pricing_edit.rateRules.meters.${key}`, { defaultValue: key })}
                </MenuItem>
              ))}
            </TextField>
            <Button
              disabled={!extra}
              onClick={() => {
                setCounts({ ...counts, [extra]: 0 });
                setExtra('');
              }}
            >
              {tr('add')}
            </Button>
          </Stack>
          <Button disabled={busy || price.type !== 'tokens'} onClick={preview}>
            {busy ? tr('running') : tr('preview')}
          </Button>
          {error && <Alert severity="error">{error}</Alert>}
          {result && (
            <>
              <Typography>{tr('previewResult')}</Typography>
              <QuotaWithDetailContent item={result} />
              {result.metadata.billing_diagnostics?.length > 0 && (
                <Alert severity="warning">{result.metadata.billing_diagnostics.join(', ')}</Alert>
              )}
            </>
          )}
        </Stack>
      </AccordionDetails>
    </Accordion>
  );
}
PriceRulesPreview.propTypes = { price: PropTypes.object.isRequired };
