import { useContext, useEffect, useState } from 'react';
import SubCard from 'ui-component/cards/SubCard';
import {
  Alert,
  Accordion,
  AccordionDetails,
  AccordionSummary,
  Box,
  Button,
  Checkbox,
  Chip,
  Divider,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  FormControl,
  FormControlLabel,
  IconButton,
  InputLabel,
  MenuItem,
  OutlinedInput,
  Select,
  Stack,
  Switch,
  TextField,
  Tooltip,
  Typography
} from '@mui/material';
import AddIcon from '@mui/icons-material/Add';
import AutoFixHighIcon from '@mui/icons-material/AutoFixHigh';
import CheckCircleOutlineIcon from '@mui/icons-material/CheckCircleOutline';
import DeleteIcon from '@mui/icons-material/DeleteOutlined';
import ExpandMoreIcon from '@mui/icons-material/ExpandMore';
import HelpOutlineIcon from '@mui/icons-material/HelpOutline';
import RestoreIcon from '@mui/icons-material/Restore';
import { showError, showSuccess, verifyJSON } from 'utils/common';
import { API } from 'utils/api';
import { AdapterDayjs } from '@mui/x-date-pickers/AdapterDayjs';
import { LocalizationProvider } from '@mui/x-date-pickers/LocalizationProvider';
import { DatePicker } from '@mui/x-date-pickers/DatePicker';
import ChatLinksDataGrid from './ChatLinksDataGrid';
import dayjs from 'dayjs';
import { LoadStatusContext } from 'contexts/StatusContext';
import { useTranslation } from 'react-i18next';
import 'dayjs/locale/zh-cn';
import { DateTimePicker } from '@mui/x-date-pickers';
import { useSelector } from 'react-redux';
import SecretOptionField from './SecretOptionField';
import {
  OPERATION_SECRET_OPTION_KEYS,
  buildSecretOptionUpdates,
  createInitialSecretStates,
  mergeSecretStatesFromMeta,
  markSecretOptionForClear,
  resetSecretOptionAction,
  updateSecretOptionDraft
} from './secretOptionState.mjs';

const defaultInputs = {
  QuotaForNewUser: 0,
  QuotaForInviter: 0,
  QuotaForInvitee: 0,
  QuotaRemindThreshold: 0,
  PreConsumedQuota: 0,
  TopUpLink: '',
  ChatLink: '',
  ChatLinks: '',
  QuotaPerUnit: 0,
  AutomaticDisableChannelEnabled: '',
  AutomaticEnableChannelEnabled: '',
  ChannelDisableThreshold: 0,
  LogConsumeEnabled: '',
  DisplayInCurrencyEnabled: '',
  ApproximateTokenEnabled: '',
  RetryTimes: 0,
  RetryStatusCodes: '',
  RetryTimeOut: 0,
  RetryCooldownSeconds: 0,
  MjNotifyEnabled: '',
  ChatImageRequestProxy: '',
  PaymentUSDRate: 0,
  PaymentMinAmount: 1,
  RechargeDiscount: '',
  CFWorkerImageUrl: '',
  CFWorkerImageKey: '',
  ClaudeAPIEnabled: '',
  GeminiAPIEnabled: '',
  DisableChannelKeywords: '',
  EnableSafe: '',
  SafeToolName: '',
  SafeKeyWords: '',
  safeTools: [],
  GeminiOpenThink: '',
  CodexRoutingHintSetting: '',
  ChannelAffinitySetting: '',
  PreferredChannelWaitMilliseconds: 0,
  PreferredChannelWaitPollMilliseconds: 50
};

const CODEX_ROUTING_HINT_RECOMMENDED = {
  prompt_cache_key_strategy: 'auto',
  model_regex: '^gpt-5$',
  user_agent_regex: ''
};

const CODEX_ROUTING_HINT_DEFAULT = {
  prompt_cache_key_strategy: 'off',
  model_regex: '',
  user_agent_regex: ''
};

const CODEX_ROUTING_HINT_DEFAULT_TEMPLATE = JSON.stringify(CODEX_ROUTING_HINT_DEFAULT, null, 2);

const DEFAULT_CHANNEL_AFFINITY_RULES = [
  {
    name: 'responses-continuation',
    enabled: true,
    kind: 'responses',
    model_regex: '',
    path_regex: '^/v1/responses(?:/compact)?$',
    user_agent_regex: '',
    include_group: true,
    include_model: false,
    include_path: false,
    include_rule_name: true,
    ignore_preferred_cooldown: false,
    strict: true,
    skip_retry_on_failure: true,
    record_on_success: true,
    ttl_seconds: '',
    key_sources: [
      {
        source: 'request_field',
        key: 'previous_response_id',
        alias: 'response_id',
        value_regex: ''
      }
    ]
  },
  {
    name: 'responses-prompt-cache-key',
    enabled: true,
    kind: 'responses',
    model_regex: '',
    path_regex: '^/v1/responses(?:/compact)?$',
    user_agent_regex: '',
    include_group: true,
    include_model: true,
    include_path: false,
    include_rule_name: true,
    ignore_preferred_cooldown: false,
    strict: false,
    skip_retry_on_failure: false,
    record_on_success: true,
    ttl_seconds: '',
    key_sources: [
      {
        source: 'request_field',
        key: 'prompt_cache_key',
        alias: 'prompt_cache_key',
        value_regex: ''
      },
      {
        source: 'request_hint',
        key: 'responses.prompt_cache_key',
        alias: 'prompt_cache_key',
        value_regex: ''
      }
    ]
  },
  {
    name: 'realtime-session',
    enabled: true,
    kind: 'realtime',
    model_regex: '',
    path_regex: '^/v1/realtime$',
    user_agent_regex: '',
    include_group: true,
    include_model: false,
    include_path: false,
    include_rule_name: true,
    ignore_preferred_cooldown: false,
    strict: false,
    skip_retry_on_failure: false,
    record_on_success: true,
    ttl_seconds: '',
    key_sources: [
      {
        source: 'header',
        key: 'x-session-id',
        alias: 'session_id',
        value_regex: ''
      },
      {
        source: 'header',
        key: 'session_id',
        alias: 'session_id',
        value_regex: ''
      }
    ]
  }
];

const DEFAULT_CHANNEL_AFFINITY = {
  enabled: true,
  default_ttl_seconds: 3600,
  max_entries: 50000,
  rules: DEFAULT_CHANNEL_AFFINITY_RULES
};

const CHANNEL_AFFINITY_DEFAULT_TEMPLATE = JSON.stringify(
  {
    enabled: true,
    default_ttl_seconds: 3600,
    max_entries: 50000
  },
  null,
  2
);

const PROMPT_CACHE_STRATEGIES = ['off', 'auto', 'session_id', 'auth_header', 'token_id', 'user_id'];
const CHANNEL_AFFINITY_KINDS = ['responses', 'realtime'];
const CHANNEL_AFFINITY_KEY_SOURCES = ['request_field', 'header', 'query', 'request_hint'];
const CHANNEL_AFFINITY_BOOLEAN_GROUPS = [
  {
    key: 'dimensions',
    fields: ['include_group', 'include_model', 'include_path', 'include_rule_name']
  },
  {
    key: 'fallback',
    fields: ['strict', 'ignore_preferred_cooldown', 'skip_retry_on_failure']
  },
  {
    key: 'writeback',
    fields: ['record_on_success']
  }
];

const cloneJSON = (value) => JSON.parse(JSON.stringify(value));

const parseJSONObjectOption = (value, fallback) => {
  if (typeof value !== 'string' || value.trim() === '') {
    return cloneJSON(fallback);
  }
  try {
    const parsed = JSON.parse(value);
    if (parsed && typeof parsed === 'object' && !Array.isArray(parsed)) {
      return parsed;
    }
  } catch (error) {
    return cloneJSON(fallback);
  }
  return cloneJSON(fallback);
};

const cleanObject = (value) => {
  if (Array.isArray(value)) {
    return value.map(cleanObject);
  }
  if (!value || typeof value !== 'object') {
    return value;
  }
  return Object.entries(value).reduce((acc, [key, raw]) => {
    const cleaned = cleanObject(raw);
    if (cleaned === '' || cleaned === null || cleaned === undefined) {
      return acc;
    }
    acc[key] = cleaned;
    return acc;
  }, {});
};

const serializeJSONObjectOption = (value) => JSON.stringify(cleanObject(value), null, 2);

const normalizeOptionalPositiveInt = (value) => {
  const trimmed = String(value ?? '').trim();
  if (trimmed === '') {
    return '';
  }
  const parsed = Number(trimmed);
  return Number.isFinite(parsed) ? parsed : value;
};

const serializeCodexRoutingHintForm = (form) => serializeJSONObjectOption(form);

const serializeChannelAffinityForm = (form) => {
  const prepared = {
    ...form,
    default_ttl_seconds: normalizeOptionalPositiveInt(form.default_ttl_seconds),
    max_entries: normalizeOptionalPositiveInt(form.max_entries),
    rules: (form.rules || []).map((rule) => ({
      ...rule,
      ttl_seconds: normalizeOptionalPositiveInt(rule.ttl_seconds),
      key_sources: rule.key_sources || []
    }))
  };
  return serializeJSONObjectOption(prepared);
};

const parseCodexRoutingHintForm = (value) => {
  const parsed = parseJSONObjectOption(value, CODEX_ROUTING_HINT_DEFAULT);
  const strategy = PROMPT_CACHE_STRATEGIES.includes(parsed.prompt_cache_key_strategy) ? parsed.prompt_cache_key_strategy : 'off';
  return {
    prompt_cache_key_strategy: strategy,
    model_regex: parsed.model_regex || '',
    user_agent_regex: parsed.user_agent_regex || ''
  };
};

const normalizeChannelAffinityKeySource = (source = {}) => ({
  source: CHANNEL_AFFINITY_KEY_SOURCES.includes(source.source) ? source.source : 'request_field',
  key: source.key || '',
  alias: source.alias || '',
  value_regex: source.value_regex || ''
});

const normalizeChannelAffinityRule = (rule = {}) => {
  const normalized = {
    name: rule.name || '',
    enabled: rule.enabled !== false,
    kind: CHANNEL_AFFINITY_KINDS.includes(rule.kind) ? rule.kind : 'responses',
    model_regex: rule.model_regex || '',
    path_regex: rule.path_regex || '',
    user_agent_regex: rule.user_agent_regex || '',
    include_group: Boolean(rule.include_group),
    include_model: Boolean(rule.include_model),
    include_path: Boolean(rule.include_path),
    include_rule_name: Boolean(rule.include_rule_name),
    ignore_preferred_cooldown: Boolean(rule.ignore_preferred_cooldown),
    strict: Boolean(rule.strict),
    skip_retry_on_failure: Boolean(rule.skip_retry_on_failure),
    record_on_success: rule.record_on_success !== false,
    ttl_seconds: rule.ttl_seconds || '',
    key_sources: Array.isArray(rule.key_sources) ? rule.key_sources.map(normalizeChannelAffinityKeySource) : []
  };
  if (normalized.key_sources.length === 0) {
    normalized.key_sources = [normalizeChannelAffinityKeySource()];
  }
  return normalized;
};

const parseChannelAffinityForm = (value) => {
  const parsed = parseJSONObjectOption(value, DEFAULT_CHANNEL_AFFINITY);
  const parsedRules = Array.isArray(parsed.rules) ? parsed.rules : DEFAULT_CHANNEL_AFFINITY_RULES;
  return {
    enabled: parsed.enabled !== false,
    default_ttl_seconds: parsed.default_ttl_seconds || 3600,
    max_entries: parsed.max_entries || 50000,
    rules: parsedRules.map(normalizeChannelAffinityRule)
  };
};

const createBlankChannelAffinityRule = () =>
  normalizeChannelAffinityRule({
    name: '',
    enabled: true,
    kind: 'responses',
    include_group: true,
    include_rule_name: true,
    record_on_success: true,
    key_sources: [normalizeChannelAffinityKeySource()]
  });

const codexHelpTopics = {
  PreferredChannelWaitMilliseconds: ['meaning', 'legalValues', 'defaultValue', 'recommended', 'impact', 'risk'],
  PreferredChannelWaitPollMilliseconds: ['meaning', 'legalValues', 'defaultValue', 'recommended', 'impact', 'risk'],
  CodexRoutingHintSetting: ['meaning', 'legalValues', 'strategies', 'filters', 'recommended', 'impact', 'risk'],
  ChannelAffinitySetting: ['meaning', 'topLevel', 'ruleFields', 'keySources', 'defaults', 'recommended', 'impact', 'risk']
};

const OperationSetting = () => {
  const { t } = useTranslation();
  const siteInfo = useSelector((state) => state.siteInfo);
  let now = new Date();
  let [inputs, setInputs] = useState(() => ({
    ...defaultInputs,
    codexRoutingHintForm: cloneJSON(CODEX_ROUTING_HINT_DEFAULT),
    channelAffinityForm: cloneJSON(DEFAULT_CHANNEL_AFFINITY),
    channelAffinityBackendDefault: false
  }));
  const [originInputs, setOriginInputs] = useState({});
  const [secretStates, setSecretStates] = useState(() => createInitialSecretStates(OPERATION_SECRET_OPTION_KEYS));
  let [loading, setLoading] = useState(false);
  const [codexHelpTopic, setCodexHelpTopic] = useState(null);
  let [historyTimestamp, setHistoryTimestamp] = useState(now.getTime() / 1000 - 30 * 24 * 3600); // a month ago new Date().getTime() / 1000 + 3600
  let [invoiceMonth, setInvoiceMonth] = useState(now.getTime()); // a month ago new Date().getTime() / 1000 + 3600
  const loadStatus = useContext(LoadStatusContext);
  const [safeToolsLoading, setSafeToolsLoading] = useState(true);

  const formatJSONObjectOption = (value) => {
    if (typeof value !== 'string') {
      return value;
    }
    if (!value.trim() || !verifyJSON(value)) {
      return value;
    }
    try {
      return JSON.stringify(JSON.parse(value), null, 2);
    } catch (error) {
      return value;
    }
  };

  const buildCodexFormInputs = (sourceInputs) => ({
    ...sourceInputs,
    codexRoutingHintForm: parseCodexRoutingHintForm(sourceInputs.CodexRoutingHintSetting),
    channelAffinityForm: parseChannelAffinityForm(sourceInputs.ChannelAffinitySetting),
    channelAffinityBackendDefault: String(sourceInputs.ChannelAffinitySetting ?? '').trim() === ''
  });

  const getOptions = async () => {
    try {
      const res = await API.get('/api/option/');
      const { success, message, data, meta } = res.data;
      if (success) {
        let newInputs = { ...defaultInputs };
        data.forEach((item) => {
          if (item.key === 'RechargeDiscount') {
            item.value = JSON.stringify(JSON.parse(item.value), null, 2);
          }
          if (item.key === 'SafeKeyWords' && typeof item.value === 'string' && item.value.startsWith('[')) {
            try {
              item.value = JSON.parse(item.value);
            } catch (e) {
              console.error('解析SafeKeyWords失败:', e);
            }
          }
          newInputs[item.key] = item.value;
        });
        newInputs.CodexRoutingHintSetting = formatJSONObjectOption(newInputs.CodexRoutingHintSetting);
        newInputs.ChannelAffinitySetting = formatJSONObjectOption(newInputs.ChannelAffinitySetting);
        // 确保不会覆盖 safeTools
        setInputs((prev) => ({ ...buildCodexFormInputs(newInputs), safeTools: prev.safeTools }));
        setOriginInputs(newInputs);
        setSecretStates(mergeSecretStatesFromMeta(OPERATION_SECRET_OPTION_KEYS, meta?.sensitive_options));
      } else {
        showError(message);
      }
    } catch (error) {
      return;
    }
  };

  const getSafeTools = async () => {
    setSafeToolsLoading(true);
    try {
      const res = await API.get('/api/option/safe_tools');
      const { success, message, data } = res.data;
      if (success) {
        setInputs((prev) => {
          const newInputs = {
            ...prev,
            safeTools: data
          };
          return newInputs;
        });
      } else {
        showError(message);
      }
    } catch (error) {
      console.error('获取安全工具列表失败:', error);
      showError('获取安全工具列表失败');
    } finally {
      setSafeToolsLoading(false);
    }
  };

  useEffect(() => {
    const initData = async () => {
      await getSafeTools();
      await getOptions();
    };
    initData();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const normalizeOptionPayloadValue = (value) => {
    if (typeof value === 'number' || typeof value === 'boolean') {
      return String(value);
    }
    return value;
  };

  const getRequestErrorMessage = (error, fallbackMessage) => {
    return error?.response?.data?.message || error?.message || fallbackMessage;
  };

  const refreshSettings = async (includeSafeTools = true) => {
    if (includeSafeTools) {
      await getSafeTools();
    }
    await getOptions();
    await loadStatus();
  };

  const putOptionOrThrow = async (key, value) => {
    const normalizedValue = normalizeOptionPayloadValue(value);

    try {
      const res = await API.put('/api/option/', {
        key,
        value: normalizedValue
      });
      const { success, message } = res.data;
      if (!success) {
        throw new Error(message || '保存失败');
      }
    } catch (error) {
      throw new Error(getRequestErrorMessage(error, '保存失败'));
    }
  };

  const putOptionBatchOrThrow = async (updates) => {
    try {
      const res = await API.put('/api/option/batch', { updates });
      const { success, message } = res.data;
      if (!success) {
        throw new Error(message || '保存失败');
      }
    } catch (error) {
      throw new Error(getRequestErrorMessage(error, '保存失败'));
    }
  };

  const buildOptionUpdates = (keys) => {
    return keys
      .filter((key) => originInputs[key] !== inputs[key])
      .map((key) => ({
        key,
        value: normalizeOptionPayloadValue(inputs[key])
      }));
  };

  const buildCodexOptionUpdates = () => {
    const codexRoutingHintSetting = serializeCodexRoutingHintForm(inputs.codexRoutingHintForm);
    const channelAffinitySetting = inputs.channelAffinityBackendDefault ? '' : serializeChannelAffinityForm(inputs.channelAffinityForm);
    const codexInputs = {
      ...inputs,
      CodexRoutingHintSetting: codexRoutingHintSetting,
      ChannelAffinitySetting: channelAffinitySetting
    };

    return [
      ...['PreferredChannelWaitMilliseconds', 'PreferredChannelWaitPollMilliseconds']
        .filter((key) => originInputs[key] !== inputs[key])
        .map((key) => ({
          key,
          value: normalizeOptionPayloadValue(inputs[key])
        })),
      ...['CodexRoutingHintSetting', 'ChannelAffinitySetting']
        .filter((key) => originInputs[key] !== codexInputs[key])
        .map((key) => ({
          key,
          value: codexInputs[key]
        }))
    ];
  };

  const isNonNegativeIntegerString = (value) => /^(0|[1-9]\d*)$/.test(String(value ?? '').trim());

  const validateCodexConfig = () => {
    if (
      originInputs.PreferredChannelWaitMilliseconds !== inputs.PreferredChannelWaitMilliseconds &&
      !isNonNegativeIntegerString(inputs.PreferredChannelWaitMilliseconds)
    ) {
      throw new Error(t('setting_index.operationSettings.codexSettings.errors.invalidWaitMilliseconds'));
    }

    if (
      originInputs.PreferredChannelWaitPollMilliseconds !== inputs.PreferredChannelWaitPollMilliseconds &&
      !isNonNegativeIntegerString(inputs.PreferredChannelWaitPollMilliseconds)
    ) {
      throw new Error(t('setting_index.operationSettings.codexSettings.errors.invalidWaitPollMilliseconds'));
    }

    if (!isNonNegativeIntegerString(inputs.channelAffinityForm.default_ttl_seconds)) {
      throw new Error(t('setting_index.operationSettings.codexSettings.errors.invalidDefaultTTL'));
    }

    if (!isNonNegativeIntegerString(inputs.channelAffinityForm.max_entries)) {
      throw new Error(t('setting_index.operationSettings.codexSettings.errors.invalidMaxEntries'));
    }

    inputs.channelAffinityForm.rules.forEach((rule, index) => {
      if (String(rule.ttl_seconds ?? '').trim() !== '' && !isNonNegativeIntegerString(rule.ttl_seconds)) {
        throw new Error(t('setting_index.operationSettings.codexSettings.errors.invalidRuleTTL', { index: index + 1 }));
      }
    });
  };

  const handleInputChange = async (event) => {
    let { name, value } = event.target;

    if (OPERATION_SECRET_OPTION_KEYS.includes(name)) {
      setSecretStates((prev) => updateSecretOptionDraft(prev, name, value));
      return;
    }
    if (name.endsWith('Enabled')) {
      setLoading(true);
      try {
        const nextValue = inputs[name] === 'true' ? 'false' : 'true';
        await putOptionOrThrow(name, nextValue);
        await refreshSettings(false);
        showSuccess('设置成功！');
      } catch (error) {
        showError(error.message || '设置失败');
      } finally {
        setLoading(false);
      }
    } else {
      setInputs((inputs) => ({ ...inputs, [name]: value }));
    }
  };

  const updateCodexRoutingHintForm = (field, value) => {
    setInputs((prev) => ({
      ...prev,
      codexRoutingHintForm: {
        ...prev.codexRoutingHintForm,
        [field]: value
      }
    }));
  };

  const updateChannelAffinityForm = (field, value) => {
    setInputs((prev) => ({
      ...prev,
      channelAffinityBackendDefault: false,
      channelAffinityForm: {
        ...prev.channelAffinityForm,
        [field]: value
      }
    }));
  };

  const updateChannelAffinityRule = (ruleIndex, field, value) => {
    setInputs((prev) => ({
      ...prev,
      channelAffinityBackendDefault: false,
      channelAffinityForm: {
        ...prev.channelAffinityForm,
        rules: prev.channelAffinityForm.rules.map((rule, index) => (index === ruleIndex ? { ...rule, [field]: value } : rule))
      }
    }));
  };

  const addChannelAffinityRule = () => {
    setInputs((prev) => ({
      ...prev,
      channelAffinityBackendDefault: false,
      channelAffinityForm: {
        ...prev.channelAffinityForm,
        rules: [...prev.channelAffinityForm.rules, createBlankChannelAffinityRule()]
      }
    }));
  };

  const removeChannelAffinityRule = (ruleIndex) => {
    setInputs((prev) => ({
      ...prev,
      channelAffinityBackendDefault: false,
      channelAffinityForm: {
        ...prev.channelAffinityForm,
        rules: prev.channelAffinityForm.rules.filter((_, index) => index !== ruleIndex)
      }
    }));
  };

  const updateChannelAffinityKeySource = (ruleIndex, sourceIndex, field, value) => {
    setInputs((prev) => ({
      ...prev,
      channelAffinityBackendDefault: false,
      channelAffinityForm: {
        ...prev.channelAffinityForm,
        rules: prev.channelAffinityForm.rules.map((rule, index) => {
          if (index !== ruleIndex) {
            return rule;
          }
          return {
            ...rule,
            key_sources: rule.key_sources.map((source, keySourceIndex) =>
              keySourceIndex === sourceIndex ? { ...source, [field]: value } : source
            )
          };
        })
      }
    }));
  };

  const addChannelAffinityKeySource = (ruleIndex) => {
    setInputs((prev) => ({
      ...prev,
      channelAffinityBackendDefault: false,
      channelAffinityForm: {
        ...prev.channelAffinityForm,
        rules: prev.channelAffinityForm.rules.map((rule, index) =>
          index === ruleIndex ? { ...rule, key_sources: [...rule.key_sources, normalizeChannelAffinityKeySource()] } : rule
        )
      }
    }));
  };

  const removeChannelAffinityKeySource = (ruleIndex, sourceIndex) => {
    setInputs((prev) => ({
      ...prev,
      channelAffinityBackendDefault: false,
      channelAffinityForm: {
        ...prev.channelAffinityForm,
        rules: prev.channelAffinityForm.rules.map((rule, index) => {
          if (index !== ruleIndex || rule.key_sources.length <= 1) {
            return rule;
          }
          return {
            ...rule,
            key_sources: rule.key_sources.filter((_, keySourceIndex) => keySourceIndex !== sourceIndex)
          };
        })
      }
    }));
  };

  const applyCodexTemplate = (name, value) => {
    setInputs((inputs) => {
      if (name === 'CodexRoutingHintSetting') {
        return { ...inputs, codexRoutingHintForm: parseCodexRoutingHintForm(value) };
      }
      if (name === 'ChannelAffinitySetting') {
        return {
          ...inputs,
          channelAffinityForm: parseChannelAffinityForm(value),
          channelAffinityBackendDefault: String(value ?? '').trim() === ''
        };
      }
      return { ...inputs, [name]: value };
    });
  };

  const applyCodexSafeDefaults = () => {
    setInputs((prev) => ({
      ...prev,
      PreferredChannelWaitMilliseconds: 0,
      PreferredChannelWaitPollMilliseconds: 50,
      codexRoutingHintForm: cloneJSON(CODEX_ROUTING_HINT_DEFAULT),
      channelAffinityForm: parseChannelAffinityForm(''),
      channelAffinityBackendDefault: true
    }));
  };

  const applyCodexRecommendedPreset = () => {
    setInputs((prev) => ({
      ...prev,
      PreferredChannelWaitMilliseconds: 250,
      PreferredChannelWaitPollMilliseconds: 50,
      codexRoutingHintForm: cloneJSON(CODEX_ROUTING_HINT_RECOMMENDED),
      channelAffinityForm: parseChannelAffinityForm(''),
      channelAffinityBackendDefault: true
    }));
  };

  const enabledAffinityRules = inputs.channelAffinityForm.rules.filter((rule) => rule.enabled).length;
  const codexHintEnabled = inputs.codexRoutingHintForm.prompt_cache_key_strategy !== 'off';
  const codexHintScope = inputs.codexRoutingHintForm.model_regex || t('setting_index.operationSettings.codexSettings.summary.allModels');
  const affinityModeLabel = inputs.channelAffinityBackendDefault
    ? t('setting_index.operationSettings.codexSettings.summary.backendDefault')
    : inputs.channelAffinityForm.enabled
      ? t('setting_index.operationSettings.codexSettings.summary.customEnabled')
      : t('setting_index.operationSettings.codexSettings.summary.customDisabled');

  const renderCodexSummaryItem = ({ label, value, detail, color = 'default' }) => (
    <Box
      sx={{
        border: '1px solid',
        borderColor: 'divider',
        borderRadius: 1,
        p: 1.5,
        minHeight: 96,
        bgcolor: 'background.default'
      }}
    >
      <Stack spacing={0.75}>
        <Typography variant="caption" color="text.secondary">
          {label}
        </Typography>
        <Chip label={value} color={color} size="small" sx={{ alignSelf: 'flex-start', fontWeight: 600 }} />
        {detail && (
          <Typography variant="body2" color="text.secondary">
            {detail}
          </Typography>
        )}
      </Stack>
    </Box>
  );

  const renderCodexFlowStep = (key, index) => (
    <Box
      key={key}
      sx={{
        border: '1px solid',
        borderColor: 'divider',
        borderRadius: 1,
        p: 1.5,
        bgcolor: index === 1 ? 'action.hover' : 'background.paper'
      }}
    >
      <Stack spacing={0.5}>
        <Chip label={index + 1} size="small" color={index === 1 ? 'primary' : 'default'} sx={{ width: 32, fontWeight: 700 }} />
        <Typography variant="subtitle2">{t(`setting_index.operationSettings.codexSettings.flow.${key}.title`)}</Typography>
        <Typography variant="body2" color="text.secondary">
          {t(`setting_index.operationSettings.codexSettings.flow.${key}.body`)}
        </Typography>
      </Stack>
    </Box>
  );

  const renderCodexQuickAction = ({ icon, title, body, onClick, color = 'primary' }) => (
    <Button
      variant="outlined"
      color={color}
      onClick={onClick}
      disabled={loading}
      startIcon={icon}
      sx={{
        justifyContent: 'flex-start',
        alignItems: 'flex-start',
        textAlign: 'left',
        py: 1.25,
        px: 1.5,
        minHeight: 92,
        whiteSpace: 'normal'
      }}
    >
      <Stack spacing={0.25}>
        <Typography variant="subtitle2" component="span">
          {title}
        </Typography>
        <Typography variant="caption" color="text.secondary" component="span">
          {body}
        </Typography>
      </Stack>
    </Button>
  );

  const renderRuleKeySourceSummary = (rule) => {
    const keySources = rule.key_sources || [];
    if (keySources.length === 0) {
      return t('setting_index.operationSettings.codexSettings.channelAffinitySetting.noKeySource');
    }
    return keySources.map((source) => `${source.source}:${source.key || '*'}${source.alias ? ` -> ${source.alias}` : ''}`).join(' / ');
  };

  const renderChannelAffinityRuleEditor = (rule, ruleIndex) => (
    <Accordion
      key={`rule-${ruleIndex}`}
      disableGutters
      sx={{
        border: '1px solid',
        borderColor: 'divider',
        borderRadius: 1,
        '&:before': { display: 'none' },
        boxShadow: 'none',
        overflow: 'hidden'
      }}
    >
      <AccordionSummary expandIcon={<ExpandMoreIcon />} sx={{ bgcolor: 'background.default' }}>
        <Stack direction={{ xs: 'column', md: 'row' }} spacing={1} alignItems={{ xs: 'flex-start', md: 'center' }} sx={{ width: '100%' }}>
          <Stack spacing={0.5} sx={{ flexGrow: 1, minWidth: 0 }}>
            <Stack direction="row" spacing={1} alignItems="center" sx={{ flexWrap: 'wrap' }}>
              <Typography variant="subtitle2" noWrap>
                {rule.name || t('setting_index.operationSettings.codexSettings.channelAffinitySetting.unnamedRule')}
              </Typography>
              <Chip label={rule.kind} size="small" variant="outlined" />
              <Chip
                label={
                  rule.enabled
                    ? t('setting_index.operationSettings.codexSettings.channelAffinitySetting.ruleEnabled')
                    : t('setting_index.operationSettings.codexSettings.channelAffinitySetting.ruleDisabled')
                }
                color={rule.enabled ? 'success' : 'default'}
                size="small"
              />
              {rule.strict && (
                <Chip
                  label={t('setting_index.operationSettings.codexSettings.channelAffinitySetting.strictBadge')}
                  color="warning"
                  size="small"
                  variant="outlined"
                />
              )}
            </Stack>
            <Typography variant="caption" color="text.secondary" noWrap>
              {renderRuleKeySourceSummary(rule)}
            </Typography>
          </Stack>
          <IconButton
            color="error"
            onClick={(event) => {
              event.stopPropagation();
              removeChannelAffinityRule(ruleIndex);
            }}
            disabled={loading || inputs.channelAffinityForm.rules.length <= 1}
            aria-label={t('setting_index.operationSettings.codexSettings.channelAffinitySetting.removeRule')}
          >
            <DeleteIcon />
          </IconButton>
        </Stack>
      </AccordionSummary>
      <AccordionDetails>
        <Stack spacing={2}>
          <Stack direction={{ xs: 'column', md: 'row' }} spacing={2} alignItems={{ xs: 'stretch', md: 'center' }}>
            <TextField
              fullWidth
              label={t('setting_index.operationSettings.codexSettings.channelAffinitySetting.fields.ruleName')}
              value={rule.name}
              onChange={(event) => updateChannelAffinityRule(ruleIndex, 'name', event.target.value)}
              disabled={loading}
            />
            <TextField
              select
              fullWidth
              label={t('setting_index.operationSettings.codexSettings.channelAffinitySetting.fields.kind')}
              value={rule.kind}
              onChange={(event) => updateChannelAffinityRule(ruleIndex, 'kind', event.target.value)}
              disabled={loading}
            >
              {CHANNEL_AFFINITY_KINDS.map((kind) => (
                <MenuItem key={kind} value={kind}>
                  {kind}
                </MenuItem>
              ))}
            </TextField>
            <FormControlLabel
              control={
                <Switch
                  checked={Boolean(rule.enabled)}
                  onChange={(event) => updateChannelAffinityRule(ruleIndex, 'enabled', event.target.checked)}
                  disabled={loading}
                />
              }
              label={t('setting_index.operationSettings.codexSettings.channelAffinitySetting.fields.enabled')}
              sx={{ flexShrink: 0, minWidth: { xs: 'auto', md: 140 } }}
            />
          </Stack>

          <Stack direction={{ xs: 'column', md: 'row' }} spacing={2}>
            <TextField
              fullWidth
              label={t('setting_index.operationSettings.codexSettings.channelAffinitySetting.fields.modelRegex')}
              value={rule.model_regex}
              onChange={(event) => updateChannelAffinityRule(ruleIndex, 'model_regex', event.target.value)}
              disabled={loading}
            />
            <TextField
              fullWidth
              label={t('setting_index.operationSettings.codexSettings.channelAffinitySetting.fields.pathRegex')}
              value={rule.path_regex}
              onChange={(event) => updateChannelAffinityRule(ruleIndex, 'path_regex', event.target.value)}
              disabled={loading}
            />
            <TextField
              fullWidth
              label={t('setting_index.operationSettings.codexSettings.channelAffinitySetting.fields.userAgentRegex')}
              value={rule.user_agent_regex}
              onChange={(event) => updateChannelAffinityRule(ruleIndex, 'user_agent_regex', event.target.value)}
              disabled={loading}
            />
          </Stack>

          <Stack spacing={1}>
            {CHANNEL_AFFINITY_BOOLEAN_GROUPS.map((group) => (
              <Box key={group.key}>
                <Typography variant="caption" color="text.secondary" sx={{ display: 'block', mb: 0.5 }}>
                  {t(`setting_index.operationSettings.codexSettings.channelAffinitySetting.booleanGroups.${group.key}`)}
                </Typography>
                <Stack direction={{ xs: 'column', md: 'row' }} spacing={1} sx={{ flexWrap: 'wrap' }}>
                  {group.fields.map((field) => (
                    <FormControlLabel
                      key={field}
                      control={
                        <Checkbox
                          checked={Boolean(rule[field])}
                          onChange={(event) => updateChannelAffinityRule(ruleIndex, field, event.target.checked)}
                          disabled={loading}
                        />
                      }
                      label={t(`setting_index.operationSettings.codexSettings.channelAffinitySetting.fields.${field}`)}
                    />
                  ))}
                </Stack>
              </Box>
            ))}
          </Stack>

          <TextField
            type="number"
            label={t('setting_index.operationSettings.codexSettings.channelAffinitySetting.fields.ruleTTL')}
            value={rule.ttl_seconds}
            onChange={(event) => updateChannelAffinityRule(ruleIndex, 'ttl_seconds', event.target.value)}
            inputProps={{ min: 0, step: 1, inputMode: 'numeric' }}
            disabled={loading}
            sx={{ maxWidth: { xs: '100%', md: 260 } }}
          />

          <Divider />
          <Stack direction={{ xs: 'column', sm: 'row' }} spacing={1} alignItems={{ xs: 'stretch', sm: 'center' }}>
            <Typography variant="subtitle2" sx={{ flexGrow: 1 }}>
              {t('setting_index.operationSettings.codexSettings.channelAffinitySetting.keySourcesTitle')}
            </Typography>
            <Button
              variant="outlined"
              size="small"
              startIcon={<AddIcon />}
              onClick={() => addChannelAffinityKeySource(ruleIndex)}
              disabled={loading}
            >
              {t('setting_index.operationSettings.codexSettings.channelAffinitySetting.addKeySource')}
            </Button>
          </Stack>

          {rule.key_sources.map((source, sourceIndex) => (
            <Stack
              key={`source-${sourceIndex}`}
              direction={{ xs: 'column', md: 'row' }}
              spacing={1.5}
              alignItems={{ xs: 'stretch', md: 'center' }}
            >
              <TextField
                select
                fullWidth
                label={t('setting_index.operationSettings.codexSettings.channelAffinitySetting.fields.source')}
                value={source.source}
                onChange={(event) => updateChannelAffinityKeySource(ruleIndex, sourceIndex, 'source', event.target.value)}
                disabled={loading}
              >
                {CHANNEL_AFFINITY_KEY_SOURCES.map((sourceOption) => (
                  <MenuItem key={sourceOption} value={sourceOption}>
                    {sourceOption}
                  </MenuItem>
                ))}
              </TextField>
              <TextField
                fullWidth
                label={t('setting_index.operationSettings.codexSettings.channelAffinitySetting.fields.key')}
                value={source.key}
                onChange={(event) => updateChannelAffinityKeySource(ruleIndex, sourceIndex, 'key', event.target.value)}
                disabled={loading}
              />
              <TextField
                fullWidth
                label={t('setting_index.operationSettings.codexSettings.channelAffinitySetting.fields.alias')}
                value={source.alias}
                onChange={(event) => updateChannelAffinityKeySource(ruleIndex, sourceIndex, 'alias', event.target.value)}
                disabled={loading}
              />
              <TextField
                fullWidth
                label={t('setting_index.operationSettings.codexSettings.channelAffinitySetting.fields.valueRegex')}
                value={source.value_regex}
                onChange={(event) => updateChannelAffinityKeySource(ruleIndex, sourceIndex, 'value_regex', event.target.value)}
                disabled={loading}
              />
              <IconButton
                color="error"
                onClick={() => removeChannelAffinityKeySource(ruleIndex, sourceIndex)}
                disabled={loading || rule.key_sources.length <= 1}
                aria-label={t('setting_index.operationSettings.codexSettings.channelAffinitySetting.removeKeySource')}
              >
                <DeleteIcon />
              </IconButton>
            </Stack>
          ))}
        </Stack>
      </AccordionDetails>
    </Accordion>
  );

  const codexHelpPrefix = 'setting_index.operationSettings.codexSettings.helpDialog';

  const openCodexHelp = (topic) => {
    setCodexHelpTopic(topic);
  };

  const closeCodexHelp = () => {
    setCodexHelpTopic(null);
  };

  const renderCodexHelpButton = (topic) => (
    <Tooltip title={t(`${codexHelpPrefix}.open`)}>
      <IconButton size="small" color="primary" onClick={() => openCodexHelp(topic)} disabled={loading}>
        <HelpOutlineIcon fontSize="inherit" />
      </IconButton>
    </Tooltip>
  );

  const renderCodexFieldTitle = (topic, label) => (
    <Stack direction="row" alignItems="center" spacing={0.5} sx={{ mb: 0.75 }}>
      <Typography variant="subtitle2">{label}</Typography>
      {renderCodexHelpButton(topic)}
    </Stack>
  );

  const renderCodexHelpDialog = () => {
    if (!codexHelpTopic) {
      return null;
    }

    return (
      <Dialog open={Boolean(codexHelpTopic)} onClose={closeCodexHelp} maxWidth="md" fullWidth>
        <DialogTitle>{t(`${codexHelpPrefix}.${codexHelpTopic}.title`)}</DialogTitle>
        <DialogContent dividers>
          <Stack spacing={2}>
            {(codexHelpTopics[codexHelpTopic] || []).map((section) => (
              <Box key={section}>
                <Typography variant="subtitle2" sx={{ mb: 0.75 }}>
                  {t(`${codexHelpPrefix}.${codexHelpTopic}.${section}.title`)}
                </Typography>
                <Typography variant="body2" sx={{ whiteSpace: 'pre-line' }}>
                  {t(`${codexHelpPrefix}.${codexHelpTopic}.${section}.body`)}
                </Typography>
              </Box>
            ))}
          </Stack>
        </DialogContent>
        <DialogActions>
          <Button onClick={closeCodexHelp}>{t(`${codexHelpPrefix}.close`)}</Button>
        </DialogActions>
      </Dialog>
    );
  };

  const handleTextFieldChange = (event) => {
    const { name, value } = event.target;
    setInputs((prev) => ({
      ...prev,
      [name]: value
    }));
  };

  const submitConfig = async (group) => {
    setLoading(true);
    try {
      switch (group) {
        case 'monitor':
          if (inputs.ChannelDisableThreshold < 0 || inputs.QuotaRemindThreshold < 0) {
            showError('最长响应时间、额度提醒阈值不能为负数');
            return;
          }
          if (originInputs['ChannelDisableThreshold'] !== inputs.ChannelDisableThreshold) {
            await putOptionOrThrow('ChannelDisableThreshold', inputs.ChannelDisableThreshold);
          }
          if (originInputs['QuotaRemindThreshold'] !== inputs.QuotaRemindThreshold) {
            await putOptionOrThrow('QuotaRemindThreshold', inputs.QuotaRemindThreshold);
          }
          break;
        case 'chatlinks':
          if (originInputs['ChatLinks'] !== inputs.ChatLinks) {
            if (!verifyJSON(inputs.ChatLinks)) {
              showError('links不是合法的 JSON 字符串');
              return;
            }
            await putOptionOrThrow('ChatLinks', inputs.ChatLinks);
          }
          break;
        case 'quota':
          if (originInputs['QuotaForNewUser'] !== inputs.QuotaForNewUser) {
            await putOptionOrThrow('QuotaForNewUser', inputs.QuotaForNewUser);
          }
          if (originInputs['QuotaForInvitee'] !== inputs.QuotaForInvitee) {
            await putOptionOrThrow('QuotaForInvitee', inputs.QuotaForInvitee);
          }
          if (originInputs['QuotaForInviter'] !== inputs.QuotaForInviter) {
            await putOptionOrThrow('QuotaForInviter', inputs.QuotaForInviter);
          }
          if (originInputs['PreConsumedQuota'] !== inputs.PreConsumedQuota) {
            await putOptionOrThrow('PreConsumedQuota', inputs.PreConsumedQuota);
          }
          break;
        case 'general':
          if (inputs.QuotaPerUnit < 0 || inputs.RetryTimes < 0 || inputs.RetryCooldownSeconds < 0 || inputs.RetryTimeOut < 0) {
            showError('单位额度、跨渠道重试次数、429 渠道冷却时间、请求重试总超时时间不能为负数');
            return;
          }

          if (originInputs['TopUpLink'] !== inputs.TopUpLink) {
            await putOptionOrThrow('TopUpLink', inputs.TopUpLink);
          }
          if (originInputs['ChatLink'] !== inputs.ChatLink) {
            await putOptionOrThrow('ChatLink', inputs.ChatLink);
          }
          if (originInputs['QuotaPerUnit'] !== inputs.QuotaPerUnit) {
            await putOptionOrThrow('QuotaPerUnit', inputs.QuotaPerUnit);
          }
          if (originInputs['RetryTimes'] !== inputs.RetryTimes) {
            await putOptionOrThrow('RetryTimes', inputs.RetryTimes);
          }
          if (originInputs['RetryStatusCodes'] !== inputs.RetryStatusCodes) {
            await putOptionOrThrow('RetryStatusCodes', inputs.RetryStatusCodes);
          }
          if (originInputs['RetryCooldownSeconds'] !== inputs.RetryCooldownSeconds) {
            await putOptionOrThrow('RetryCooldownSeconds', inputs.RetryCooldownSeconds);
          }
          if (originInputs['RetryTimeOut'] !== inputs.RetryTimeOut) {
            await putOptionOrThrow('RetryTimeOut', inputs.RetryTimeOut);
          }
          break;
        case 'other':
          {
            const updates = [
              ...buildOptionUpdates(['ChatImageRequestProxy', 'CFWorkerImageUrl']),
              ...buildSecretOptionUpdates(['CFWorkerImageKey'], secretStates)
            ];
            if (updates.length > 0) {
              await putOptionBatchOrThrow(updates);
            }
          }
          break;
        case 'payment':
          if (originInputs['PaymentUSDRate'] !== inputs.PaymentUSDRate) {
            await putOptionOrThrow('PaymentUSDRate', inputs.PaymentUSDRate);
          }
          if (originInputs['PaymentMinAmount'] !== inputs.PaymentMinAmount) {
            await putOptionOrThrow('PaymentMinAmount', inputs.PaymentMinAmount);
          }
          if (originInputs['RechargeDiscount'] !== inputs.RechargeDiscount) {
            try {
              if (!verifyJSON(inputs.RechargeDiscount)) {
                showError('固定金额充值折扣不是合法的 JSON 字符串');
                return;
              }
              await putOptionOrThrow('RechargeDiscount', inputs.RechargeDiscount);
            } catch (error) {
              showError('固定金额充值折扣处理失败: ' + error.message);
              return;
            }
          }
          break;
        case 'DisableChannelKeywords':
          if (originInputs.DisableChannelKeywords !== inputs.DisableChannelKeywords) {
            // DisableChannelKeywords 已经是字符串格式，无需解析
            await putOptionOrThrow('DisableChannelKeywords', inputs.DisableChannelKeywords);
          }
          break;
        case 'safety':
          try {
            if (originInputs.EnableSafe !== inputs.EnableSafe) {
              await putOptionOrThrow('EnableSafe', inputs.EnableSafe);
            }
            if (originInputs.SafeToolName !== inputs.SafeToolName) {
              await putOptionOrThrow('SafeToolName', inputs.SafeToolName);
            }
            if (originInputs.SafeKeyWords !== inputs.SafeKeyWords) {
              await putOptionOrThrow('SafeKeyWords', inputs.SafeKeyWords);
            }
          } catch (error) {
            console.error('安全设置提交错误:', error);
            showError(`安全设置保存失败: ${error.message || '未知错误'}`);
            setLoading(false);
            return;
          }
          break;
        case 'gemini':
          if (originInputs.GeminiOpenThink !== inputs.GeminiOpenThink) {
            if (!verifyJSON(inputs.GeminiOpenThink)) {
              showError('GeminiOpenThink 不是合法的 JSON 字符串');
              return;
            }
            await putOptionOrThrow('GeminiOpenThink', inputs.GeminiOpenThink);
          }
          break;
        case 'codex':
          validateCodexConfig();
          await putOptionBatchOrThrow(buildCodexOptionUpdates());
          break;
      }

      await refreshSettings();
      showSuccess('保存成功！');
    } catch (error) {
      showError('保存失败：' + (error.message || '未知错误'));
    } finally {
      setLoading(false);
    }
  };

  const deleteHistoryLogs = async () => {
    try {
      const res = await API.delete(`/api/log/?target_timestamp=${Math.floor(historyTimestamp)}`);
      const { success, message, data } = res.data;
      if (success) {
        showSuccess(`${data} 条日志已清理！`);
        return;
      }
      showError('日志清理失败：' + message);
    } catch (error) {
      return;
    }
  };

  const genInvoiceMonth = async () => {
    try {
      const time = dayjs(invoiceMonth).format('YYYY-MM-DD');
      const res = await API.post(`/api/option/invoice/gen/${time}`);
      const { success, message } = res.data;
      if (success) {
        showSuccess(`账单生成成功！`);
        return;
      }
      showError('账单生成失败：' + message);
    } catch (error) {
      return;
    }
  };
  const updateInvoiceMonth = async () => {
    try {
      const time = dayjs(invoiceMonth).format('YYYY-MM-DD');
      const res = await API.post(`/api/option/invoice/update/${time}`);
      const { success, message } = res.data;
      if (success) {
        showSuccess(`账单更新成功！`);
        return;
      }
      showError('账单更新失败：' + message);
    } catch (error) {
      return;
    }
  };

  const handleSecretClear = (key) => {
    setSecretStates((prev) => markSecretOptionForClear(prev, key));
  };

  const handleSecretReset = (key) => {
    setSecretStates((prev) => resetSecretOptionAction(prev, key));
  };

  return (
    <Stack spacing={2}>
      <SubCard title={t('setting_index.operationSettings.generalSettings.title')}>
        <Stack justifyContent="flex-start" alignItems="flex-start" spacing={2}>
          <Stack direction={{ sm: 'column', md: 'row' }} spacing={{ xs: 3, sm: 2, md: 4 }}>
            <FormControl fullWidth>
              <InputLabel htmlFor="TopUpLink">{t('setting_index.operationSettings.generalSettings.topUpLink.label')}</InputLabel>
              <OutlinedInput
                id="TopUpLink"
                name="TopUpLink"
                value={inputs.TopUpLink}
                onChange={handleInputChange}
                label={t('setting_index.operationSettings.generalSettings.topUpLink.label')}
                placeholder={t('setting_index.operationSettings.generalSettings.topUpLink.placeholder')}
                disabled={loading}
              />
            </FormControl>
            <FormControl fullWidth>
              <InputLabel htmlFor="ChatLink">{t('setting_index.operationSettings.generalSettings.chatLink.label')}</InputLabel>
              <OutlinedInput
                id="ChatLink"
                name="ChatLink"
                value={inputs.ChatLink}
                onChange={handleInputChange}
                label={t('setting_index.operationSettings.generalSettings.chatLink.label')}
                placeholder={t('setting_index.operationSettings.generalSettings.chatLink.placeholder')}
                disabled={loading}
              />
            </FormControl>
            <FormControl fullWidth>
              <InputLabel htmlFor="QuotaPerUnit">{t('setting_index.operationSettings.generalSettings.quotaPerUnit.label')}</InputLabel>
              <OutlinedInput
                id="QuotaPerUnit"
                name="QuotaPerUnit"
                value={inputs.QuotaPerUnit}
                onChange={handleInputChange}
                label={t('setting_index.operationSettings.generalSettings.quotaPerUnit.label')}
                placeholder={t('setting_index.operationSettings.generalSettings.quotaPerUnit.placeholder')}
                disabled={loading}
              />
            </FormControl>
          </Stack>
          <Stack direction={{ sm: 'column', md: 'row' }} spacing={{ xs: 3, sm: 2, md: 4 }} sx={{ width: '100%' }}>
            <FormControl fullWidth>
              <InputLabel htmlFor="RetryTimes">{t('setting_index.operationSettings.generalSettings.retryTimes.label')}</InputLabel>
              <OutlinedInput
                id="RetryTimes"
                name="RetryTimes"
                value={inputs.RetryTimes}
                onChange={handleInputChange}
                label={t('setting_index.operationSettings.generalSettings.retryTimes.label')}
                placeholder={t('setting_index.operationSettings.generalSettings.retryTimes.placeholder')}
                disabled={loading}
              />
            </FormControl>
            <FormControl fullWidth>
              <InputLabel htmlFor="RetryStatusCodes">
                {t('setting_index.operationSettings.generalSettings.retryStatusCodes.label')}
              </InputLabel>
              <OutlinedInput
                id="RetryStatusCodes"
                name="RetryStatusCodes"
                value={inputs.RetryStatusCodes}
                onChange={handleInputChange}
                label={t('setting_index.operationSettings.generalSettings.retryStatusCodes.label')}
                placeholder={t('setting_index.operationSettings.generalSettings.retryStatusCodes.placeholder')}
                disabled={loading}
              />
            </FormControl>
            <FormControl fullWidth>
              <InputLabel htmlFor="RetryCooldownSeconds">
                {t('setting_index.operationSettings.generalSettings.retryCooldownSeconds.label')}
              </InputLabel>
              <OutlinedInput
                id="RetryCooldownSeconds"
                name="RetryCooldownSeconds"
                value={inputs.RetryCooldownSeconds}
                onChange={handleInputChange}
                label={t('setting_index.operationSettings.generalSettings.retryCooldownSeconds.label')}
                placeholder={t('setting_index.operationSettings.generalSettings.retryCooldownSeconds.placeholder')}
                disabled={loading}
              />
            </FormControl>
            <FormControl fullWidth>
              <InputLabel htmlFor="RetryTimeOut">{t('setting_index.operationSettings.generalSettings.retryTimeOut.label')}</InputLabel>
              <OutlinedInput
                id="RetryTimeOut"
                name="RetryTimeOut"
                value={inputs.RetryTimeOut}
                onChange={handleInputChange}
                label={t('setting_index.operationSettings.generalSettings.retryTimeOut.label')}
                placeholder={t('setting_index.operationSettings.generalSettings.retryTimeOut.placeholder')}
                disabled={loading}
              />
            </FormControl>
          </Stack>
          <Stack
            direction={{ sm: 'column', md: 'row' }}
            spacing={{ xs: 3, sm: 2, md: 4 }}
            justifyContent="flex-start"
            alignItems="flex-start"
          >
            <FormControlLabel
              sx={{ marginLeft: '0px' }}
              label={t('setting_index.operationSettings.generalSettings.displayInCurrency')}
              control={
                <Checkbox
                  checked={inputs.DisplayInCurrencyEnabled === 'true'}
                  onChange={handleInputChange}
                  name="DisplayInCurrencyEnabled"
                />
              }
            />

            <FormControlLabel
              label={t('setting_index.operationSettings.generalSettings.approximateToken')}
              control={
                <Checkbox checked={inputs.ApproximateTokenEnabled === 'true'} onChange={handleInputChange} name="ApproximateTokenEnabled" />
              }
            />
          </Stack>
          <Button
            variant="contained"
            onClick={() => {
              submitConfig('general').then();
            }}
          >
            {t('setting_index.operationSettings.generalSettings.saveButton')}
          </Button>
        </Stack>
      </SubCard>
      <SubCard title={t('setting_index.operationSettings.otherSettings.title')}>
        <Stack justifyContent="flex-start" alignItems="flex-start" spacing={2}>
          <Stack
            direction={{ sm: 'column', md: 'row' }}
            spacing={{ xs: 3, sm: 2, md: 4 }}
            justifyContent="flex-start"
            alignItems="flex-start"
          >
            <FormControlLabel
              sx={{ marginLeft: '0px' }}
              label={t('setting_index.operationSettings.otherSettings.mjNotify')}
              control={<Checkbox checked={inputs.MjNotifyEnabled === 'true'} onChange={handleInputChange} name="MjNotifyEnabled" />}
            />
            <FormControlLabel
              sx={{ marginLeft: '0px' }}
              label={t('setting_index.operationSettings.otherSettings.claudeAPIEnabled')}
              control={<Checkbox checked={inputs.ClaudeAPIEnabled === 'true'} onChange={handleInputChange} name="ClaudeAPIEnabled" />}
            />
            <FormControlLabel
              sx={{ marginLeft: '0px' }}
              label={t('setting_index.operationSettings.otherSettings.geminiAPIEnabled')}
              control={<Checkbox checked={inputs.GeminiAPIEnabled === 'true'} onChange={handleInputChange} name="GeminiAPIEnabled" />}
            />
          </Stack>
          <Stack spacing={2}>
            <Alert severity="info">{t('setting_index.operationSettings.otherSettings.alert')}</Alert>
            <FormControl>
              <InputLabel htmlFor="ChatImageRequestProxy">
                {t('setting_index.operationSettings.otherSettings.chatImageRequestProxy.label')}
              </InputLabel>
              <OutlinedInput
                id="ChatImageRequestProxy"
                name="ChatImageRequestProxy"
                value={inputs.ChatImageRequestProxy}
                onChange={handleInputChange}
                label={t('setting_index.operationSettings.otherSettings.chatImageRequestProxy.label')}
                placeholder={t('setting_index.operationSettings.otherSettings.chatImageRequestProxy.placeholder')}
                disabled={loading}
              />
            </FormControl>
          </Stack>

          <Stack spacing={2}>
            <Alert severity="info">{t('setting_index.operationSettings.otherSettings.CFWorkerImageUrl.alert')}</Alert>
            <FormControl>
              <InputLabel htmlFor="CFWorkerImageUrl">
                {t('setting_index.operationSettings.otherSettings.CFWorkerImageUrl.label')}
              </InputLabel>
              <OutlinedInput
                id="CFWorkerImageUrl"
                name="CFWorkerImageUrl"
                value={inputs.CFWorkerImageUrl}
                onChange={handleInputChange}
                label={t('setting_index.operationSettings.otherSettings.CFWorkerImageUrl.label')}
                placeholder={t('setting_index.operationSettings.otherSettings.CFWorkerImageUrl.label')}
                disabled={loading}
              />
            </FormControl>

            <SecretOptionField
              id="CFWorkerImageKey"
              name="CFWorkerImageKey"
              label={t('setting_index.operationSettings.otherSettings.CFWorkerImageUrl.key')}
              placeholder={t('setting_index.operationSettings.otherSettings.CFWorkerImageUrl.key')}
              secretState={secretStates.CFWorkerImageKey}
              onChange={handleInputChange}
              onClear={() => handleSecretClear('CFWorkerImageKey')}
              onReset={() => handleSecretReset('CFWorkerImageKey')}
              disabled={loading}
              t={t}
            />
          </Stack>
          <Button
            variant="contained"
            onClick={() => {
              submitConfig('other').then();
            }}
          >
            {t('setting_index.operationSettings.otherSettings.saveButton')}
          </Button>
        </Stack>
      </SubCard>
      <SubCard title={t('setting_index.operationSettings.logSettings.title')}>
        <Stack direction="column" justifyContent="flex-start" alignItems="flex-start" spacing={2}>
          <FormControlLabel
            label={t('setting_index.operationSettings.logSettings.logConsume')}
            control={<Checkbox checked={inputs.LogConsumeEnabled === 'true'} onChange={handleInputChange} name="LogConsumeEnabled" />}
          />
          <FormControl>
            <LocalizationProvider dateAdapter={AdapterDayjs} adapterLocale={'zh-cn'}>
              <DateTimePicker
                label={t('setting_index.operationSettings.logSettings.logCleanupTime.label')}
                placeholder={t('setting_index.operationSettings.logSettings.logCleanupTime.placeholder')}
                ampm={false}
                name="historyTimestamp"
                value={historyTimestamp === null ? null : dayjs.unix(historyTimestamp)}
                disabled={loading}
                onChange={(newValue) => {
                  setHistoryTimestamp(newValue === null ? null : newValue.unix());
                }}
                slotProps={{
                  actionBar: {
                    actions: ['today', 'clear', 'accept']
                  }
                }}
              />
            </LocalizationProvider>
          </FormControl>
          <Button
            variant="contained"
            onClick={() => {
              deleteHistoryLogs().then();
            }}
          >
            {t('setting_index.operationSettings.logSettings.clearLogs')}
          </Button>
        </Stack>
      </SubCard>

      {siteInfo.UserInvoiceMonth && (
        <SubCard title={t('setting_index.operationSettings.invoice.title')}>
          <Stack direction="column" justifyContent="flex-start" alignItems="flex-start" spacing={2}>
            <FormControl>
              <LocalizationProvider dateAdapter={AdapterDayjs} adapterLocale={'zh-cn'}>
                <DatePicker
                  label={t('setting_index.operationSettings.invoice.genTime')}
                  placeholder={t('setting_index.operationSettings.invoice.genTime')}
                  name="invoiceMonth"
                  value={invoiceMonth === null ? null : dayjs(invoiceMonth)}
                  disabled={loading}
                  views={['month', 'year']}
                  format="YYYY-MM"
                  onChange={(newValue) => {
                    // Set to the first day of the selected month
                    if (newValue) {
                      const firstDayOfMonth = newValue.startOf('month');
                      setInvoiceMonth(firstDayOfMonth.valueOf());
                    } else {
                      setInvoiceMonth(null);
                    }
                  }}
                  slotProps={{
                    actionBar: {
                      actions: ['clear', 'accept']
                    }
                  }}
                />
              </LocalizationProvider>
            </FormControl>
            <Stack direction={{ xs: 'column', sm: 'row' }} spacing={2} sx={{ width: '100%' }}>
              <Button
                variant="contained"
                color="success"
                sx={{ width: { xs: '100%', sm: 'auto' } }}
                onClick={() => {
                  if (invoiceMonth) {
                    genInvoiceMonth().then();
                  } else {
                    showError('Please select invoice Month');
                  }
                }}
              >
                {t('setting_index.operationSettings.invoice.genMonthInvoice')}
              </Button>
              <Button
                variant="contained"
                color="warning"
                sx={{ width: { xs: '100%', sm: 'auto' } }}
                onClick={() => {
                  if (invoiceMonth) {
                    updateInvoiceMonth().then();
                  } else {
                    showError('Please select invoice Month');
                  }
                }}
              >
                {t('setting_index.operationSettings.invoice.updateMonthInvoice')}
              </Button>
            </Stack>
          </Stack>
        </SubCard>
      )}

      <SubCard title={t('setting_index.operationSettings.monitoringSettings.title')}>
        <Stack justifyContent="flex-start" alignItems="flex-start" spacing={2}>
          <Stack direction={{ sm: 'column', md: 'row' }} spacing={{ xs: 3, sm: 2, md: 4 }}>
            <FormControl fullWidth>
              <InputLabel htmlFor="ChannelDisableThreshold">
                {t('setting_index.operationSettings.monitoringSettings.channelDisableThreshold.label')}
              </InputLabel>
              <OutlinedInput
                id="ChannelDisableThreshold"
                name="ChannelDisableThreshold"
                type="number"
                value={inputs.ChannelDisableThreshold}
                onChange={handleInputChange}
                label={t('setting_index.operationSettings.monitoringSettings.channelDisableThreshold.label')}
                placeholder={t('setting_index.operationSettings.monitoringSettings.channelDisableThreshold.placeholder')}
                disabled={loading}
              />
            </FormControl>
            <FormControl fullWidth>
              <InputLabel htmlFor="QuotaRemindThreshold">
                {t('setting_index.operationSettings.monitoringSettings.quotaRemindThreshold.label')}
              </InputLabel>
              <OutlinedInput
                id="QuotaRemindThreshold"
                name="QuotaRemindThreshold"
                type="number"
                value={inputs.QuotaRemindThreshold}
                onChange={handleInputChange}
                label={t('setting_index.operationSettings.monitoringSettings.quotaRemindThreshold.label')}
                placeholder={t('setting_index.operationSettings.monitoringSettings.quotaRemindThreshold.placeholder')}
                disabled={loading}
              />
            </FormControl>
          </Stack>
          <FormControlLabel
            label={t('setting_index.operationSettings.monitoringSettings.automaticDisableChannel')}
            control={
              <Checkbox
                checked={inputs.AutomaticDisableChannelEnabled === 'true'}
                onChange={handleInputChange}
                name="AutomaticDisableChannelEnabled"
              />
            }
          />
          <FormControlLabel
            label={t('setting_index.operationSettings.monitoringSettings.automaticEnableChannel')}
            control={
              <Checkbox
                checked={inputs.AutomaticEnableChannelEnabled === 'true'}
                onChange={handleInputChange}
                name="AutomaticEnableChannelEnabled"
              />
            }
          />
          <Alert severity="info">{t('setting_index.operationSettings.monitoringSettings.automaticEnableChannelTip')}</Alert>
          <Button
            variant="contained"
            onClick={() => {
              submitConfig('monitor').then();
            }}
          >
            {t('setting_index.operationSettings.monitoringSettings.saveMonitoringSettings')}
          </Button>
        </Stack>
      </SubCard>
      <SubCard title={t('setting_index.operationSettings.quotaSettings.title')}>
        <Stack justifyContent="flex-start" alignItems="flex-start" spacing={2}>
          <Stack direction={{ sm: 'column', md: 'row' }} spacing={{ xs: 3, sm: 2, md: 4 }}>
            <FormControl fullWidth>
              <InputLabel htmlFor="QuotaForNewUser">{t('setting_index.operationSettings.quotaSettings.quotaForNewUser.label')}</InputLabel>
              <OutlinedInput
                id="QuotaForNewUser"
                name="QuotaForNewUser"
                type="number"
                value={inputs.QuotaForNewUser}
                onChange={handleInputChange}
                label={t('setting_index.operationSettings.quotaSettings.quotaForNewUser.label')}
                placeholder={t('setting_index.operationSettings.quotaSettings.quotaForNewUser.placeholder')}
                disabled={loading}
              />
            </FormControl>
            <FormControl fullWidth>
              <InputLabel htmlFor="PreConsumedQuota">
                {t('setting_index.operationSettings.quotaSettings.preConsumedQuota.label')}
              </InputLabel>
              <OutlinedInput
                id="PreConsumedQuota"
                name="PreConsumedQuota"
                type="number"
                value={inputs.PreConsumedQuota}
                onChange={handleInputChange}
                label={t('setting_index.operationSettings.quotaSettings.preConsumedQuota.label')}
                placeholder={t('setting_index.operationSettings.quotaSettings.preConsumedQuota.placeholder')}
                disabled={loading}
              />
            </FormControl>
            <FormControl fullWidth>
              <InputLabel htmlFor="QuotaForInviter">{t('setting_index.operationSettings.quotaSettings.quotaForInviter.label')}</InputLabel>
              <OutlinedInput
                id="QuotaForInviter"
                name="QuotaForInviter"
                type="number"
                label={t('setting_index.operationSettings.quotaSettings.quotaForInviter.label')}
                value={inputs.QuotaForInviter}
                onChange={handleInputChange}
                placeholder={t('setting_index.operationSettings.quotaSettings.quotaForInviter.placeholder')}
                disabled={loading}
              />
            </FormControl>
            <FormControl fullWidth>
              <InputLabel htmlFor="QuotaForInvitee">{t('setting_index.operationSettings.quotaSettings.quotaForInvitee.label')}</InputLabel>
              <OutlinedInput
                id="QuotaForInvitee"
                name="QuotaForInvitee"
                type="number"
                label={t('setting_index.operationSettings.quotaSettings.quotaForInvitee.label')}
                value={inputs.QuotaForInvitee}
                onChange={handleInputChange}
                autoComplete="new-password"
                placeholder={t('setting_index.operationSettings.quotaSettings.quotaForInvitee.placeholder')}
                disabled={loading}
              />
            </FormControl>
          </Stack>
          <Button
            variant="contained"
            onClick={() => {
              submitConfig('quota').then();
            }}
          >
            {t('setting_index.operationSettings.quotaSettings.saveQuotaSettings')}
          </Button>
        </Stack>
      </SubCard>
      <SubCard title={t('setting_index.operationSettings.paymentSettings.title')}>
        <Stack justifyContent="flex-start" alignItems="flex-start" spacing={2}>
          <Stack justifyContent="flex-start" alignItems="flex-start" spacing={2}>
            <FormControl fullWidth>
              <Alert severity="info">
                <div dangerouslySetInnerHTML={{ __html: t('setting_index.operationSettings.paymentSettings.alert') }} />
              </Alert>
            </FormControl>
            <Stack direction={{ sm: 'column', md: 'row' }} spacing={{ xs: 3, sm: 2, md: 4 }}>
              <FormControl fullWidth>
                <InputLabel htmlFor="PaymentUSDRate">{t('setting_index.operationSettings.paymentSettings.usdRate.label')}</InputLabel>
                <OutlinedInput
                  id="PaymentUSDRate"
                  name="PaymentUSDRate"
                  type="number"
                  value={inputs.PaymentUSDRate}
                  onChange={handleInputChange}
                  label={t('setting_index.operationSettings.paymentSettings.usdRate.label')}
                  placeholder={t('setting_index.operationSettings.paymentSettings.usdRate.placeholder')}
                  disabled={loading}
                />
              </FormControl>
              <FormControl fullWidth>
                <InputLabel htmlFor="PaymentMinAmount">{t('setting_index.operationSettings.paymentSettings.minAmount.label')}</InputLabel>
                <OutlinedInput
                  id="PaymentMinAmount"
                  name="PaymentMinAmount"
                  type="number"
                  value={inputs.PaymentMinAmount}
                  onChange={handleInputChange}
                  label={t('setting_index.operationSettings.paymentSettings.minAmount.label')}
                  placeholder={t('setting_index.operationSettings.paymentSettings.minAmount.placeholder')}
                  disabled={loading}
                />
              </FormControl>
            </Stack>
          </Stack>
          <Stack spacing={2}>
            <Alert severity="info">
              <div dangerouslySetInnerHTML={{ __html: t('setting_index.operationSettings.paymentSettings.discountInfo') }} />
            </Alert>
            <FormControl fullWidth>
              <TextField
                multiline
                maxRows={15}
                id="channel-RechargeDiscount-label"
                label={t('setting_index.operationSettings.paymentSettings.discount.label')}
                value={inputs.RechargeDiscount}
                name="RechargeDiscount"
                onChange={handleTextFieldChange}
                aria-describedby="helper-text-channel-RechargeDiscount-label"
                minRows={5}
                placeholder={t('setting_index.operationSettings.paymentSettings.discount.placeholder')}
                disabled={loading}
              />
            </FormControl>
          </Stack>
          <Button
            variant="contained"
            onClick={() => {
              submitConfig('payment').then();
            }}
          >
            {t('setting_index.operationSettings.paymentSettings.save')}
          </Button>
        </Stack>
      </SubCard>

      <SubCard title={t('setting_index.operationSettings.chatLinkSettings.title')}>
        <Stack spacing={2}>
          <Alert severity="info">
            <div dangerouslySetInnerHTML={{ __html: t('setting_index.operationSettings.chatLinkSettings.info') }} />
          </Alert>
          <Stack justifyContent="flex-start" alignItems="flex-start" spacing={2}>
            <ChatLinksDataGrid links={inputs.ChatLinks || '[]'} onChange={handleInputChange} />

            <Button
              variant="contained"
              onClick={() => {
                submitConfig('chatlinks').then();
              }}
            >
              {t('setting_index.operationSettings.chatLinkSettings.save')}
            </Button>
          </Stack>
        </Stack>
      </SubCard>

      <SubCard title={t('setting_index.operationSettings.disableChannelKeywordsSettings.title')}>
        <Stack spacing={2}>
          <Stack justifyContent="flex-start" alignItems="flex-start" spacing={2}>
            <FormControl fullWidth>
              <TextField
                multiline
                maxRows={15}
                id="disableChannelKeywords"
                label={t('setting_index.operationSettings.disableChannelKeywordsSettings.info')}
                value={inputs.DisableChannelKeywords}
                name="DisableChannelKeywords"
                onChange={handleTextFieldChange}
                minRows={5}
                placeholder={t('setting_index.operationSettings.disableChannelKeywordsSettings.info')}
                disabled={loading}
              />
            </FormControl>
            <Button
              variant="contained"
              onClick={() => {
                submitConfig('DisableChannelKeywords').then();
              }}
            >
              {t('setting_index.operationSettings.disableChannelKeywordsSettings.save')}
            </Button>
          </Stack>
        </Stack>
      </SubCard>

      <SubCard title={t('setting_index.operationSettings.geminiSettings.title')}>
        <Stack spacing={2}>
          <Stack justifyContent="flex-start" alignItems="flex-start" spacing={2}>
            <FormControl fullWidth>
              <TextField
                multiline
                maxRows={15}
                id="GeminiOpenThink"
                label={t('setting_index.operationSettings.geminiSettings.geminiOpenThink.label')}
                value={inputs.GeminiOpenThink}
                name="GeminiOpenThink"
                onChange={handleTextFieldChange}
                minRows={5}
                placeholder={t('setting_index.operationSettings.geminiSettings.geminiOpenThink.placeholder')}
                disabled={loading}
              />
            </FormControl>

            <Button
              variant="contained"
              onClick={() => {
                submitConfig('gemini').then();
              }}
            >
              {t('setting_index.operationSettings.geminiSettings.save')}
            </Button>
          </Stack>
        </Stack>
      </SubCard>

      <SubCard title={t('setting_index.operationSettings.codexSettings.title')}>
        <Stack spacing={2.5}>
          <Box
            sx={{
              border: '1px solid',
              borderColor: 'divider',
              borderRadius: 1,
              p: { xs: 2, md: 2.5 },
              bgcolor: 'background.default'
            }}
          >
            <Stack spacing={2}>
              <Stack direction={{ xs: 'column', md: 'row' }} spacing={2} alignItems={{ xs: 'stretch', md: 'flex-start' }}>
                <Stack spacing={0.75} sx={{ flexGrow: 1 }}>
                  <Chip
                    label={t('setting_index.operationSettings.codexSettings.globalOptionBadge')}
                    size="small"
                    color="info"
                    variant="outlined"
                    sx={{ alignSelf: 'flex-start', fontWeight: 600 }}
                  />
                  <Typography variant="h4" sx={{ fontWeight: 700 }}>
                    {t('setting_index.operationSettings.codexSettings.decisionTitle')}
                  </Typography>
                  <Typography variant="body2" color="text.secondary">
                    {t('setting_index.operationSettings.codexSettings.description')}
                  </Typography>
                </Stack>
                <Box
                  sx={{
                    display: 'grid',
                    gridTemplateColumns: { xs: '1fr', sm: 'repeat(2, minmax(0, 1fr))', md: 'repeat(2, minmax(220px, 1fr))' },
                    gap: 1.25,
                    minWidth: { md: 460 }
                  }}
                >
                  {renderCodexQuickAction({
                    icon: <AutoFixHighIcon />,
                    title: t('setting_index.operationSettings.codexSettings.quickActions.recommended.title'),
                    body: t('setting_index.operationSettings.codexSettings.quickActions.recommended.body'),
                    onClick: applyCodexRecommendedPreset
                  })}
                  {renderCodexQuickAction({
                    icon: <RestoreIcon />,
                    title: t('setting_index.operationSettings.codexSettings.quickActions.safeDefault.title'),
                    body: t('setting_index.operationSettings.codexSettings.quickActions.safeDefault.body'),
                    onClick: applyCodexSafeDefaults,
                    color: 'inherit'
                  })}
                </Box>
              </Stack>
              <Alert severity="warning">{t('setting_index.operationSettings.codexSettings.globalWarning')}</Alert>
            </Stack>
          </Box>

          <Box
            sx={{
              display: 'grid',
              gridTemplateColumns: { xs: '1fr', md: 'repeat(4, minmax(0, 1fr))' },
              gap: 1.5
            }}
          >
            {renderCodexSummaryItem({
              label: t('setting_index.operationSettings.codexSettings.summary.affinityMode'),
              value: affinityModeLabel,
              detail: t('setting_index.operationSettings.codexSettings.summary.ruleCount', {
                count: enabledAffinityRules,
                total: inputs.channelAffinityForm.rules.length
              }),
              color: inputs.channelAffinityBackendDefault ? 'success' : inputs.channelAffinityForm.enabled ? 'warning' : 'error'
            })}
            {renderCodexSummaryItem({
              label: t('setting_index.operationSettings.codexSettings.summary.hintStrategy'),
              value: inputs.codexRoutingHintForm.prompt_cache_key_strategy,
              detail: t('setting_index.operationSettings.codexSettings.summary.hintScope', { scope: codexHintScope }),
              color: codexHintEnabled ? 'success' : 'default'
            })}
            {renderCodexSummaryItem({
              label: t('setting_index.operationSettings.codexSettings.summary.waitBudget'),
              value: `${inputs.PreferredChannelWaitMilliseconds || 0} / ${inputs.PreferredChannelWaitPollMilliseconds || 50} ms`,
              detail: t('setting_index.operationSettings.codexSettings.summary.waitBudgetDetail'),
              color: Number(inputs.PreferredChannelWaitMilliseconds) > 0 ? 'warning' : 'default'
            })}
            {renderCodexSummaryItem({
              label: t('setting_index.operationSettings.codexSettings.summary.storage'),
              value: t('setting_index.operationSettings.codexSettings.summary.globalOption'),
              detail: t('setting_index.operationSettings.codexSettings.summary.storageDetail'),
              color: 'info'
            })}
          </Box>

          <Box
            sx={{
              display: 'grid',
              gridTemplateColumns: { xs: '1fr', md: 'repeat(3, minmax(0, 1fr))' },
              gap: 1.5
            }}
          >
            {['request', 'hint', 'affinity'].map(renderCodexFlowStep)}
          </Box>

          <Box
            sx={{
              display: 'grid',
              gridTemplateColumns: { xs: '1fr', lg: 'minmax(0, 1fr) minmax(0, 1fr)' },
              gap: 2
            }}
          >
            <Box sx={{ border: '1px solid', borderColor: 'divider', borderRadius: 1, p: 2 }}>
              <Stack spacing={2}>
                <Stack direction="row" alignItems="center" justifyContent="space-between" spacing={1}>
                  {renderCodexFieldTitle(
                    'ChannelAffinitySetting',
                    t('setting_index.operationSettings.codexSettings.channelAffinitySetting.label')
                  )}
                  <Chip
                    label={
                      inputs.channelAffinityBackendDefault
                        ? t('setting_index.operationSettings.codexSettings.summary.backendDefault')
                        : t('setting_index.operationSettings.codexSettings.summary.customOverride')
                    }
                    color={inputs.channelAffinityBackendDefault ? 'success' : 'warning'}
                    size="small"
                    variant="outlined"
                  />
                </Stack>
                <Typography variant="body2" color="text.secondary">
                  {t('setting_index.operationSettings.codexSettings.channelAffinitySetting.help')}
                </Typography>
                <Stack direction={{ xs: 'column', sm: 'row' }} spacing={1}>
                  <Button
                    variant="outlined"
                    size="small"
                    startIcon={<CheckCircleOutlineIcon />}
                    onClick={() => applyCodexTemplate('ChannelAffinitySetting', '')}
                    disabled={loading}
                  >
                    {t('setting_index.operationSettings.codexSettings.channelAffinitySetting.useBlankDefault')}
                  </Button>
                  <Button
                    variant="outlined"
                    size="small"
                    startIcon={<AutoFixHighIcon />}
                    onClick={() => applyCodexTemplate('ChannelAffinitySetting', CHANNEL_AFFINITY_DEFAULT_TEMPLATE)}
                    disabled={loading}
                  >
                    {t('setting_index.operationSettings.codexSettings.channelAffinitySetting.useDefault')}
                  </Button>
                </Stack>
                {inputs.channelAffinityBackendDefault ? (
                  <Alert severity="success">
                    {t('setting_index.operationSettings.codexSettings.channelAffinitySetting.backendDefaultAlert')}
                  </Alert>
                ) : (
                  <Alert severity="warning">{t('setting_index.operationSettings.codexSettings.channelAffinitySetting.customAlert')}</Alert>
                )}
                <Stack direction={{ xs: 'column', md: 'row' }} spacing={2} alignItems={{ xs: 'stretch', md: 'center' }}>
                  <FormControlLabel
                    control={
                      <Switch
                        checked={Boolean(inputs.channelAffinityForm.enabled)}
                        onChange={(event) => updateChannelAffinityForm('enabled', event.target.checked)}
                        disabled={loading}
                      />
                    }
                    label={t('setting_index.operationSettings.codexSettings.channelAffinitySetting.fields.enabled')}
                  />
                  <TextField
                    fullWidth
                    type="number"
                    label={t('setting_index.operationSettings.codexSettings.channelAffinitySetting.fields.defaultTTL')}
                    value={inputs.channelAffinityForm.default_ttl_seconds}
                    onChange={(event) => updateChannelAffinityForm('default_ttl_seconds', event.target.value)}
                    inputProps={{ min: 0, step: 1, inputMode: 'numeric' }}
                    disabled={loading}
                  />
                  <TextField
                    fullWidth
                    type="number"
                    label={t('setting_index.operationSettings.codexSettings.channelAffinitySetting.fields.maxEntries')}
                    value={inputs.channelAffinityForm.max_entries}
                    onChange={(event) => updateChannelAffinityForm('max_entries', event.target.value)}
                    inputProps={{ min: 0, step: 1, inputMode: 'numeric' }}
                    disabled={loading}
                  />
                </Stack>
              </Stack>
            </Box>

            <Box sx={{ border: '1px solid', borderColor: 'divider', borderRadius: 1, p: 2 }}>
              <Stack spacing={2}>
                {renderCodexFieldTitle(
                  'CodexRoutingHintSetting',
                  t('setting_index.operationSettings.codexSettings.codexRoutingHintSetting.label')
                )}
                <Typography variant="body2" color="text.secondary">
                  {t('setting_index.operationSettings.codexSettings.codexRoutingHintSetting.help')}
                </Typography>
                <Stack direction={{ xs: 'column', sm: 'row' }} spacing={1}>
                  <Button
                    variant="outlined"
                    size="small"
                    startIcon={<AutoFixHighIcon />}
                    onClick={() => applyCodexTemplate('CodexRoutingHintSetting', JSON.stringify(CODEX_ROUTING_HINT_RECOMMENDED, null, 2))}
                    disabled={loading}
                  >
                    {t('setting_index.operationSettings.codexSettings.codexRoutingHintSetting.useRecommended')}
                  </Button>
                  <Button
                    variant="outlined"
                    size="small"
                    startIcon={<RestoreIcon />}
                    onClick={() => applyCodexTemplate('CodexRoutingHintSetting', CODEX_ROUTING_HINT_DEFAULT_TEMPLATE)}
                    disabled={loading}
                  >
                    {t('setting_index.operationSettings.codexSettings.codexRoutingHintSetting.useDefault')}
                  </Button>
                </Stack>
                <TextField
                  select
                  fullWidth
                  id="CodexRoutingHintStrategy"
                  label={t('setting_index.operationSettings.codexSettings.codexRoutingHintSetting.fields.strategy')}
                  value={inputs.codexRoutingHintForm.prompt_cache_key_strategy}
                  onChange={(event) => updateCodexRoutingHintForm('prompt_cache_key_strategy', event.target.value)}
                  disabled={loading}
                  SelectProps={{ renderValue: (value) => value }}
                >
                  {PROMPT_CACHE_STRATEGIES.map((strategy) => (
                    <MenuItem key={strategy} value={strategy}>
                      <Stack spacing={0.25}>
                        <Typography variant="body2">{strategy}</Typography>
                        <Typography variant="caption" color="text.secondary">
                          {t(`setting_index.operationSettings.codexSettings.codexRoutingHintSetting.strategyDescriptions.${strategy}`)}
                        </Typography>
                      </Stack>
                    </MenuItem>
                  ))}
                </TextField>
                <Stack direction={{ xs: 'column', md: 'row' }} spacing={2}>
                  <TextField
                    fullWidth
                    id="CodexRoutingHintModelRegex"
                    label={t('setting_index.operationSettings.codexSettings.codexRoutingHintSetting.fields.modelRegex')}
                    value={inputs.codexRoutingHintForm.model_regex}
                    onChange={(event) => updateCodexRoutingHintForm('model_regex', event.target.value)}
                    disabled={loading}
                  />
                  <TextField
                    fullWidth
                    id="CodexRoutingHintUserAgentRegex"
                    label={t('setting_index.operationSettings.codexSettings.codexRoutingHintSetting.fields.userAgentRegex')}
                    value={inputs.codexRoutingHintForm.user_agent_regex}
                    onChange={(event) => updateCodexRoutingHintForm('user_agent_regex', event.target.value)}
                    disabled={loading}
                  />
                </Stack>
              </Stack>
            </Box>
          </Box>

          <Box sx={{ border: '1px solid', borderColor: 'divider', borderRadius: 1, p: 2 }}>
            <Stack spacing={2}>
              <Typography variant="subtitle1" sx={{ fontWeight: 700 }}>
                {t('setting_index.operationSettings.codexSettings.waitSectionTitle')}
              </Typography>
              <Stack direction={{ sm: 'column', md: 'row' }} spacing={{ xs: 3, sm: 2, md: 4 }}>
                <FormControl fullWidth>
                  {renderCodexFieldTitle(
                    'PreferredChannelWaitMilliseconds',
                    t('setting_index.operationSettings.codexSettings.preferredChannelWaitMilliseconds.label')
                  )}
                  <OutlinedInput
                    id="PreferredChannelWaitMilliseconds"
                    name="PreferredChannelWaitMilliseconds"
                    type="number"
                    value={inputs.PreferredChannelWaitMilliseconds}
                    onChange={handleInputChange}
                    placeholder={t('setting_index.operationSettings.codexSettings.preferredChannelWaitMilliseconds.placeholder')}
                    inputProps={{
                      min: 0,
                      step: 1,
                      inputMode: 'numeric',
                      'aria-label': t('setting_index.operationSettings.codexSettings.preferredChannelWaitMilliseconds.label')
                    }}
                    disabled={loading}
                  />
                  <Typography variant="caption" color="text.secondary" sx={{ mt: 0.75 }}>
                    {t('setting_index.operationSettings.codexSettings.preferredChannelWaitMilliseconds.help')}
                  </Typography>
                </FormControl>
                <FormControl fullWidth>
                  {renderCodexFieldTitle(
                    'PreferredChannelWaitPollMilliseconds',
                    t('setting_index.operationSettings.codexSettings.preferredChannelWaitPollMilliseconds.label')
                  )}
                  <OutlinedInput
                    id="PreferredChannelWaitPollMilliseconds"
                    name="PreferredChannelWaitPollMilliseconds"
                    type="number"
                    value={inputs.PreferredChannelWaitPollMilliseconds}
                    onChange={handleInputChange}
                    placeholder={t('setting_index.operationSettings.codexSettings.preferredChannelWaitPollMilliseconds.placeholder')}
                    inputProps={{
                      min: 0,
                      step: 1,
                      inputMode: 'numeric',
                      'aria-label': t('setting_index.operationSettings.codexSettings.preferredChannelWaitPollMilliseconds.label')
                    }}
                    disabled={loading}
                  />
                  <Typography variant="caption" color="text.secondary" sx={{ mt: 0.75 }}>
                    {t('setting_index.operationSettings.codexSettings.preferredChannelWaitPollMilliseconds.help')}
                  </Typography>
                </FormControl>
              </Stack>
            </Stack>
          </Box>

          {/* Trade-off: keep the low-risk configuration path visible and move full rule editing behind one advanced section. */}
          <Accordion
            disableGutters
            sx={{
              border: '1px solid',
              borderColor: 'divider',
              borderRadius: 1,
              '&:before': { display: 'none' },
              boxShadow: 'none',
              overflow: 'hidden'
            }}
          >
            <AccordionSummary expandIcon={<ExpandMoreIcon />} sx={{ bgcolor: 'background.default' }}>
              <Stack
                direction={{ xs: 'column', md: 'row' }}
                spacing={1}
                alignItems={{ xs: 'flex-start', md: 'center' }}
                sx={{ width: '100%' }}
              >
                <Stack spacing={0.25} sx={{ flexGrow: 1 }}>
                  <Typography variant="subtitle1" sx={{ fontWeight: 700 }}>
                    {t('setting_index.operationSettings.codexSettings.channelAffinitySetting.advancedTitle')}
                  </Typography>
                  <Typography variant="body2" color="text.secondary">
                    {t('setting_index.operationSettings.codexSettings.channelAffinitySetting.advancedDescription')}
                  </Typography>
                </Stack>
                <Chip
                  label={t('setting_index.operationSettings.codexSettings.summary.ruleCount', {
                    count: enabledAffinityRules,
                    total: inputs.channelAffinityForm.rules.length
                  })}
                  size="small"
                  variant="outlined"
                />
              </Stack>
            </AccordionSummary>
            <AccordionDetails>
              <Stack spacing={2}>
                {inputs.channelAffinityForm.rules.length === 0 && (
                  <Alert severity="error">
                    {t('setting_index.operationSettings.codexSettings.channelAffinitySetting.emptyRulesWarning')}
                  </Alert>
                )}
                <Stack direction={{ xs: 'column', sm: 'row' }} spacing={1} alignItems={{ xs: 'stretch', sm: 'center' }}>
                  <Typography variant="body2" color="text.secondary" sx={{ flexGrow: 1 }}>
                    {t('setting_index.operationSettings.codexSettings.channelAffinitySetting.rulesHelp')}
                  </Typography>
                  <Button variant="outlined" size="small" startIcon={<AddIcon />} onClick={addChannelAffinityRule} disabled={loading}>
                    {t('setting_index.operationSettings.codexSettings.channelAffinitySetting.addRule')}
                  </Button>
                </Stack>
                <Stack spacing={1.25}>{inputs.channelAffinityForm.rules.map(renderChannelAffinityRuleEditor)}</Stack>
              </Stack>
            </AccordionDetails>
          </Accordion>

          <Button
            variant="contained"
            onClick={() => {
              submitConfig('codex').then();
            }}
          >
            {t('setting_index.operationSettings.codexSettings.save')}
          </Button>
          {renderCodexHelpDialog()}
        </Stack>
      </SubCard>

      <SubCard title={t('setting_index.operationSettings.safetySettings.title')}>
        <Stack spacing={2}>
          <Stack justifyContent="flex-start" alignItems="flex-start" spacing={2}>
            <FormControlLabel
              label={
                <Stack direction="row" alignItems="center" spacing={1}>
                  <span>{t('setting_index.operationSettings.safetySettings.enableSafe')}</span>
                  <Chip
                    label="Beta"
                    size="small"
                    color="error"
                    sx={{
                      height: '20px',
                      fontSize: '0.75rem',
                      fontWeight: 'bold',
                      backgroundColor: 'red',
                      color: 'white'
                    }}
                  />
                </Stack>
              }
              control={
                <Checkbox
                  checked={inputs.EnableSafe === 'true'}
                  onChange={(e) => {
                    console.log('Checkbox changed:', e.target.checked);
                    const newValue = e.target.checked ? 'true' : 'false';
                    console.log('Setting EnableSafe to:', newValue);
                    setInputs((prev) => ({
                      ...prev,
                      EnableSafe: newValue
                    }));
                  }}
                />
              }
            />

            <FormControl fullWidth>
              <InputLabel htmlFor="SafeToolName">{t('setting_index.operationSettings.safetySettings.safeToolName.label')}</InputLabel>
              <Select
                id="SafeToolName"
                name="SafeToolName"
                value={inputs.SafeToolName || ''}
                label={t('setting_index.operationSettings.safetySettings.safeToolName.label')}
                disabled={loading || safeToolsLoading}
                onChange={(e) => {
                  setInputs((prev) => ({
                    ...prev,
                    SafeToolName: e.target.value
                  }));
                }}
              >
                {safeToolsLoading && <MenuItem value="">加载中...</MenuItem>}
                {!safeToolsLoading && (!inputs.safeTools || inputs.safeTools.length === 0) && <MenuItem value="">暂无安全工具</MenuItem>}
                {inputs.safeTools &&
                  inputs.safeTools.map((tool) => (
                    <MenuItem key={tool} value={tool}>
                      {tool}
                    </MenuItem>
                  ))}
              </Select>
            </FormControl>

            <FormControl fullWidth>
              <TextField
                multiline
                maxRows={15}
                id="SafeKeyWords"
                label={t('setting_index.operationSettings.safetySettings.safeKeyWords.label')}
                value={Array.isArray(inputs.SafeKeyWords) ? inputs.SafeKeyWords.join('\n') : inputs.SafeKeyWords}
                name="SafeKeyWords"
                onChange={handleTextFieldChange}
                onKeyDown={(e) => {
                  if (e.key === 'Enter' && !e.shiftKey) {
                    e.stopPropagation();
                  }
                }}
                minRows={5}
                placeholder={t('setting_index.operationSettings.safetySettings.safeKeyWords.placeholder')}
                disabled={loading}
              />
            </FormControl>

            <Button
              variant="contained"
              onClick={() => {
                submitConfig('safety').then();
              }}
            >
              {t('setting_index.operationSettings.safetySettings.save')}
            </Button>
          </Stack>
        </Stack>
      </SubCard>
    </Stack>
  );
};

export default OperationSetting;
