import PropTypes from 'prop-types';
import { useId, useState } from 'react';
import {
  Alert,
  Autocomplete,
  Accordion,
  AccordionSummary,
  AccordionDetails,
  Box,
  Button,
  Chip,
  MenuItem,
  Stack,
  TextField,
  Typography
} from '@mui/material';
import { useTranslation } from 'react-i18next';
import {
  RATE_RULE_GROUPS,
  RATE_EXTRA_KEYS,
  validateRateRules,
  getRateGroupRules,
  rateRuleEntries,
  formatRuleCondition
} from './rateRulesState.mjs';

export default function RateRulesEditor({ value = {}, onChange }) {
  const { t } = useTranslation();
  const tr = (key, options) => t(`pricing_edit.rateRules.${key}`, options);
  const errors = validateRateRules(value);
  const scheduleHeadingId = useId();
  const update = (group, rules) =>
    onChange(
      group === 'schedule' ? { ...value, version: 2, schedule: { ...value.schedule, rules } } : { ...value, version: 2, [group]: rules }
    );
  const updateRule = (group, index, next) =>
    update(
      group,
      getRateGroupRules(value, group).map((rule, i) => (i === index ? next : rule))
    );
  const add = (group) => {
    const ids = new Set(rateRuleEntries(value).map(([, rule]) => rule.id));
    let number = 1;
    while (ids.has(`${group}-${number}`)) number++;
    const when = {
      service_tier: { service_tier: ['flex'] },
      speed: { speed: ['fast'] },
      long_context: { input_tokens: { gt: 200000 } },
      schedule: { time: { start: '23:00', end: '07:00' } }
    }[group];
    update(group, [...getRateGroupRules(value, group), { id: `${group}-${number}`, when, multipliers: { all: 1 } }]);
  };
  return (
    <Stack spacing={2}>
      <Typography variant="subtitle2">{tr('title')}</Typography>
      <Alert severity="info">{tr('scope')}</Alert>
      <Typography variant="body2">{tr('composition')}</Typography>
      {RATE_RULE_GROUPS.map((group) => {
        const rows = getRateGroupRules(value, group);
        const addRuleButton = (
          <Button aria-label={`${tr('addRule')} · ${tr(group)}`} onClick={() => add(group)}>
            {tr('addRule')}
          </Button>
        );
        const content = (
          <>
            {group === 'schedule' && (
              <Stack spacing={1} sx={{ my: 1 }}>
                <Typography variant="body2">{tr('schedulePriority')}</Typography>
                <TextField
                  label={tr('timezone')}
                  value={value.schedule?.timezone || ''}
                  placeholder="Asia/Shanghai"
                  error={!!errors['schedule.timezone']}
                  helperText={errors['schedule.timezone'] ? tr('invalid') : tr('timezoneHelp')}
                  onChange={(event) => {
                    const schedule = { ...value.schedule };
                    if (event.target.value) schedule.timezone = event.target.value;
                    else delete schedule.timezone;
                    onChange({ ...value, version: 2, schedule });
                  }}
                />
              </Stack>
            )}
            {rows.map((rule, index) => {
              const label = formatRuleCondition(group, rule.when, tr);
              const error = Object.entries(errors).find(([path]) => path.startsWith(`${group}.${index}`))?.[1];
              return (
                <Accordion key={rule.id} defaultExpanded>
                  <AccordionSummary expandIcon={<span aria-hidden="true">⌄</span>}>
                    <Stack direction="row" spacing={1}>
                      {group === 'schedule' && <Chip size="small" label={tr('priority', { value: index + 1 })} />}
                      <Typography>{label}</Typography>
                    </Stack>
                  </AccordionSummary>
                  <AccordionDetails>
                    <Stack spacing={2}>
                      <ConditionEditor group={group} value={rule.when} onChange={(when) => updateRule(group, index, { ...rule, when })} />
                      <MultiplierEditor
                        value={rule.multipliers}
                        onChange={(multipliers) => updateRule(group, index, { ...rule, multipliers })}
                      />
                      {error && <Alert severity="error">{tr(error)}</Alert>}
                      <Stack direction="row" spacing={1}>
                        {group === 'schedule' &&
                          [-1, 1].map((direction) => (
                            <Button
                              key={direction}
                              disabled={index + direction < 0 || index + direction >= rows.length}
                              aria-label={`${tr(direction < 0 ? 'raisePriority' : 'lowerPriority')} · ${label}`}
                              onClick={() => {
                                const next = [...rows];
                                [next[index], next[index + direction]] = [next[index + direction], next[index]];
                                update(group, next);
                              }}
                            >
                              {tr(direction < 0 ? 'raisePriority' : 'lowerPriority')}
                            </Button>
                          ))}
                        <Button
                          color="error"
                          onClick={() =>
                            update(
                              group,
                              rows.filter((_, i) => i !== index)
                            )
                          }
                        >
                          {tr('remove')}
                        </Button>
                      </Stack>
                    </Stack>
                  </AccordionDetails>
                </Accordion>
              );
            })}
          </>
        );
        return (
          <Box key={group} component="section" aria-label={tr(group)}>
            {group === 'schedule' ? (
              <Accordion defaultExpanded={rows.length > 0}>
                <AccordionSummary
                  id={scheduleHeadingId}
                  aria-controls={`${scheduleHeadingId}-content`}
                  expandIcon={<span aria-hidden="true">⌄</span>}
                >
                  <Typography variant="subtitle2">{tr('schedule')}</Typography>
                </AccordionSummary>
                <AccordionDetails>
                  <Stack spacing={1}>
                    <Box sx={{ alignSelf: 'flex-end' }}>{addRuleButton}</Box>
                    {content}
                  </Stack>
                </AccordionDetails>
              </Accordion>
            ) : (
              <>
                <Stack direction="row" alignItems="center" justifyContent="space-between">
                  <Typography variant="subtitle2">{tr(group)}</Typography>
                  {addRuleButton}
                </Stack>
                {content}
              </>
            )}
          </Box>
        );
      })}
      {(errors.rules || errors.schedule) && <Alert severity="error">{tr('invalid')}</Alert>}
    </Stack>
  );
}

function ConditionEditor({ group, value = {}, onChange }) {
  const { t } = useTranslation();
  const tr = (key) => t(`pricing_edit.rateRules.${key}`);
  const set = (key, next) => {
    const result = { ...value };
    if (next === undefined) delete result[key];
    else result[key] = next;
    onChange(result);
  };
  const enumInput = (key, optional = false) => (
    <Autocomplete
      multiple
      freeSolo
      fullWidth
      options={key === 'speed' ? ['fast', 'standard'] : ['default', 'flex', 'priority', 'fast']}
      value={value[key] || []}
      onChange={(_, next) => set(key, next.length ? next : undefined)}
      renderInput={(params) => (
        <TextField
          {...params}
          label={tr(optional ? 'applicableSpeed' : key)}
          helperText={tr(optional ? 'applicableSpeedHelp' : 'enumHelp')}
        />
      )}
    />
  );
  if (group === 'service_tier' || group === 'speed') return enumInput(group);
  if (group === 'long_context')
    return (
      <Stack spacing={1.5}>
        <Stack direction={{ xs: 'column', sm: 'row' }} spacing={1}>
          {['gt', 'lte'].map((key) => (
            <TextField
              fullWidth
              key={key}
              label={tr(key)}
              type="number"
              value={value.input_tokens?.[key] ?? ''}
              inputProps={{ min: 0, step: 1 }}
              onChange={(event) => {
                const range = { ...value.input_tokens };
                if (event.target.value === '') delete range[key];
                else range[key] = event.target.value;
                set('input_tokens', Object.keys(range).length ? range : undefined);
              }}
            />
          ))}
        </Stack>
        <Accordion disableGutters>
          <AccordionSummary expandIcon={<span aria-hidden="true">⌄</span>}>{tr('applicableSpeed')}</AccordionSummary>
          <AccordionDetails>{enumInput('speed', true)}</AccordionDetails>
        </Accordion>
      </Stack>
    );
  return (
    <Stack spacing={1.5}>
      <TextField
        select
        SelectProps={{ multiple: true }}
        label={tr('weekdays')}
        value={value.weekdays || []}
        helperText={tr('weekdayHelp')}
        onChange={(event) => set('weekdays', event.target.value.length ? event.target.value : undefined)}
      >
        {[1, 2, 3, 4, 5, 6, 7].map((day) => (
          <MenuItem key={day} value={day}>
            {tr(`day${day}`)}
          </MenuItem>
        ))}
      </TextField>
      <Stack direction={{ xs: 'column', sm: 'row' }} spacing={1}>
        {['start', 'end'].map((key) => (
          <TextField
            fullWidth
            key={key}
            label={tr(key)}
            type="time"
            InputLabelProps={{ shrink: true }}
            value={value.time?.[key] || ''}
            onChange={(event) => {
              const next = { ...value.time, [key]: event.target.value };
              set('time', next.start || next.end ? next : undefined);
            }}
          />
        ))}
        <Button onClick={() => set('time', undefined)}>{tr('allDay')}</Button>
      </Stack>
    </Stack>
  );
}

function MultiplierEditor({ value = {}, onChange }) {
  const { t } = useTranslation();
  const [extra, setExtra] = useState('');
  const tr = (key, options) => t(`pricing_edit.rateRules.${key}`, options);
  const set = (key, raw) => {
    const next = { ...value };
    if (raw === '') delete next[key];
    else next[key] = raw;
    onChange(next);
  };
  const input = value.input ?? value.all ?? 1;
  const output = value.output ?? value.all ?? 1;
  const side = (key) => (key.startsWith('output_') || key === 'reasoning_tokens' ? output : input);
  return (
    <Stack spacing={1}>
      <TextField
        label={tr('all')}
        type="number"
        inputProps={{ min: 0, step: 'any' }}
        value={value.all ?? ''}
        placeholder="1"
        helperText={tr('inheritHelp')}
        onChange={(event) => set('all', event.target.value)}
      />
      <Accordion disableGutters>
        <AccordionSummary expandIcon={<span aria-hidden="true">⌄</span>}>{tr('overrides')}</AccordionSummary>
        <AccordionDetails>
          <Stack spacing={1.5}>
            {['input', 'output'].map((key) => (
              <TextField
                key={key}
                label={tr(`${key}Multiplier`)}
                type="number"
                inputProps={{ min: 0, step: 'any' }}
                value={value[key] ?? ''}
                placeholder={String(value.all ?? 1)}
                helperText={tr('inherit', { value: value.all ?? 1 })}
                onChange={(event) => set(key, event.target.value)}
              />
            ))}
            {Object.entries(value.extra_multipliers || {}).map(([key, amount]) => (
              <Stack key={key} direction="row" spacing={1}>
                <TextField
                  fullWidth
                  label={t(`pricing_edit.rateRules.meters.${key}`, { defaultValue: key })}
                  type="number"
                  inputProps={{ min: 0, step: 'any' }}
                  value={amount}
                  helperText={tr('inherit', { value: side(key) })}
                  onChange={(event) => onChange({ ...value, extra_multipliers: { ...value.extra_multipliers, [key]: event.target.value } })}
                />
                <Button
                  onClick={() => {
                    const next = { ...value.extra_multipliers };
                    delete next[key];
                    onChange({ ...value, extra_multipliers: next });
                  }}
                >
                  {tr('follow')}
                </Button>
              </Stack>
            ))}
            <Stack direction="row" spacing={1}>
              <TextField fullWidth select label={tr('extra')} value={extra} onChange={(event) => setExtra(event.target.value)}>
                <MenuItem value="">{tr('selectExtra')}</MenuItem>
                {RATE_EXTRA_KEYS.filter((key) => value.extra_multipliers?.[key] === undefined).map((key) => (
                  <MenuItem key={key} value={key}>
                    {t(`pricing_edit.rateRules.meters.${key}`, { defaultValue: key })}
                  </MenuItem>
                ))}
              </TextField>
              <Button
                disabled={!extra}
                onClick={() => {
                  onChange({ ...value, extra_multipliers: { ...value.extra_multipliers, [extra]: 1 } });
                  setExtra('');
                }}
              >
                {tr('add')}
              </Button>
            </Stack>
          </Stack>
        </AccordionDetails>
      </Accordion>
    </Stack>
  );
}

RateRulesEditor.propTypes = { value: PropTypes.object, onChange: PropTypes.func.isRequired };
ConditionEditor.propTypes = { ...RateRulesEditor.propTypes, group: PropTypes.oneOf(RATE_RULE_GROUPS).isRequired };
MultiplierEditor.propTypes = RateRulesEditor.propTypes;
