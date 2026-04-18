import { useContext, useEffect, useState } from 'react';
import SubCard from 'ui-component/cards/SubCard';
import {
  Alert,
  Button,
  Checkbox,
  Chip,
  FormControl,
  FormControlLabel,
  InputLabel,
  MenuItem,
  OutlinedInput,
  Select,
  Stack,
  TextField
} from '@mui/material';
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

const OperationSetting = () => {
  const { t } = useTranslation();
  const siteInfo = useSelector((state) => state.siteInfo);
  let now = new Date();
  let [inputs, setInputs] = useState(() => ({ ...defaultInputs }));
  const [originInputs, setOriginInputs] = useState({});
  const [secretStates, setSecretStates] = useState(() => createInitialSecretStates(OPERATION_SECRET_OPTION_KEYS));
  let [loading, setLoading] = useState(false);
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
          if (item.key === 'CodexRoutingHintSetting' || item.key === 'ChannelAffinitySetting') {
            item.value = formatJSONObjectOption(item.value);
          }
          newInputs[item.key] = item.value;
        });
        // 确保不会覆盖 safeTools
        setInputs((prev) => ({ ...newInputs, safeTools: prev.safeTools }));
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

    if (
      originInputs.CodexRoutingHintSetting !== inputs.CodexRoutingHintSetting &&
      inputs.CodexRoutingHintSetting.trim() !== '' &&
      !verifyJSON(inputs.CodexRoutingHintSetting)
    ) {
      throw new Error(t('setting_index.operationSettings.codexSettings.errors.invalidRoutingHintJson'));
    }

    if (
      originInputs.ChannelAffinitySetting !== inputs.ChannelAffinitySetting &&
      inputs.ChannelAffinitySetting.trim() !== '' &&
      !verifyJSON(inputs.ChannelAffinitySetting)
    ) {
      throw new Error(t('setting_index.operationSettings.codexSettings.errors.invalidChannelAffinityJson'));
    }
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
            showError('单位额度、重试次数、冷却时间、重试超时时间不能为负数');
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
          await putOptionBatchOrThrow(
            buildOptionUpdates([
              'PreferredChannelWaitMilliseconds',
              'PreferredChannelWaitPollMilliseconds',
              'CodexRoutingHintSetting',
              'ChannelAffinitySetting'
            ])
          );
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
            <Stack direction="row" spacing={2}>
              <Button
                variant="contained"
                color="success"
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
        <Stack spacing={2}>
          <Alert severity="info">{t('setting_index.operationSettings.codexSettings.description')}</Alert>
          <Stack direction={{ sm: 'column', md: 'row' }} spacing={{ xs: 3, sm: 2, md: 4 }}>
            <FormControl fullWidth>
              <InputLabel htmlFor="PreferredChannelWaitMilliseconds">
                {t('setting_index.operationSettings.codexSettings.preferredChannelWaitMilliseconds.label')}
              </InputLabel>
              <OutlinedInput
                id="PreferredChannelWaitMilliseconds"
                name="PreferredChannelWaitMilliseconds"
                type="number"
                value={inputs.PreferredChannelWaitMilliseconds}
                onChange={handleInputChange}
                label={t('setting_index.operationSettings.codexSettings.preferredChannelWaitMilliseconds.label')}
                placeholder={t('setting_index.operationSettings.codexSettings.preferredChannelWaitMilliseconds.placeholder')}
                inputProps={{ min: 0, step: 1, inputMode: 'numeric' }}
                disabled={loading}
              />
            </FormControl>
            <FormControl fullWidth>
              <InputLabel htmlFor="PreferredChannelWaitPollMilliseconds">
                {t('setting_index.operationSettings.codexSettings.preferredChannelWaitPollMilliseconds.label')}
              </InputLabel>
              <OutlinedInput
                id="PreferredChannelWaitPollMilliseconds"
                name="PreferredChannelWaitPollMilliseconds"
                type="number"
                value={inputs.PreferredChannelWaitPollMilliseconds}
                onChange={handleInputChange}
                label={t('setting_index.operationSettings.codexSettings.preferredChannelWaitPollMilliseconds.label')}
                placeholder={t('setting_index.operationSettings.codexSettings.preferredChannelWaitPollMilliseconds.placeholder')}
                inputProps={{ min: 0, step: 1, inputMode: 'numeric' }}
                disabled={loading}
              />
            </FormControl>
          </Stack>
          <FormControl fullWidth>
            <TextField
              multiline
              maxRows={20}
              id="CodexRoutingHintSetting"
              label={t('setting_index.operationSettings.codexSettings.codexRoutingHintSetting.label')}
              value={inputs.CodexRoutingHintSetting}
              name="CodexRoutingHintSetting"
              onChange={handleTextFieldChange}
              minRows={6}
              placeholder={t('setting_index.operationSettings.codexSettings.codexRoutingHintSetting.placeholder')}
              disabled={loading}
            />
          </FormControl>
          <FormControl fullWidth>
            <TextField
              multiline
              maxRows={24}
              id="ChannelAffinitySetting"
              label={t('setting_index.operationSettings.codexSettings.channelAffinitySetting.label')}
              value={inputs.ChannelAffinitySetting}
              name="ChannelAffinitySetting"
              onChange={handleTextFieldChange}
              minRows={10}
              placeholder={t('setting_index.operationSettings.codexSettings.channelAffinitySetting.placeholder')}
              disabled={loading}
            />
          </FormControl>
          <Button
            variant="contained"
            onClick={() => {
              submitConfig('codex').then();
            }}
          >
            {t('setting_index.operationSettings.codexSettings.save')}
          </Button>
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
