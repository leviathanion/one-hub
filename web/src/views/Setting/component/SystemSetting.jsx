import { useState, useEffect, useContext } from 'react';
import SubCard from 'ui-component/cards/SubCard';
import {
  Stack,
  FormControl,
  InputLabel,
  OutlinedInput,
  Checkbox,
  Button,
  FormControlLabel,
  Dialog,
  DialogTitle,
  DialogContent,
  DialogActions,
  Divider,
  Alert,
  Autocomplete,
  TextField
} from '@mui/material';
import Grid from '@mui/material/Unstable_Grid2';
import { showError, showSuccess, removeTrailingSlash } from 'utils/common'; //,
import { API } from 'utils/api';
import { createFilterOptions } from '@mui/material/Autocomplete';
import { LoadStatusContext } from 'contexts/StatusContext';
import { useTranslation } from 'react-i18next';
import SecretOptionField from './SecretOptionField';
import {
  SYSTEM_SECRET_OPTION_KEYS,
  buildSecretOptionUpdates,
  createInitialSecretStates,
  getSecretClearBlockedMessage,
  mergeSecretStatesFromMeta,
  markSecretOptionForClear,
  resetSecretOptionAction,
  updateSecretOptionDraft
} from './secretOptionState.mjs';
import {
  buildSystemProviderSectionUpdates,
  getSystemProviderSectionConfig,
  SYSTEM_PROVIDER_TOGGLE_KEYS
} from './systemProviderSections.mjs';

const filter = createFilterOptions();
const defaultInputs = {
  PasswordLoginEnabled: '',
  PasswordRegisterEnabled: '',
  EmailVerificationEnabled: '',
  GitHubOAuthEnabled: '',
  GitHubClientId: '',
  GitHubClientSecret: '',
  GitHubOldIdCloseEnabled: '',
  LarkAuthEnabled: '',
  LarkClientId: '',
  LarkClientSecret: '',
  OIDCAuthEnabled: '',
  OIDCClientId: '',
  OIDCClientSecret: '',
  OIDCIssuer: '',
  OIDCScopes: '',
  OIDCUsernameClaims: '',
  Notice: '',
  SMTPServer: '',
  SMTPPort: '',
  SMTPAccount: '',
  SMTPFrom: '',
  SMTPToken: '',
  ServerAddress: '',
  Footer: '',
  WeChatAuthEnabled: '',
  WeChatServerAddress: '',
  WeChatServerToken: '',
  WeChatAccountQRCodeImageURL: '',
  TurnstileCheckEnabled: '',
  TurnstileSiteKey: '',
  TurnstileSecretKey: '',
  RegisterEnabled: '',
  EmailDomainRestrictionEnabled: '',
  EmailDomainWhitelist: ''
};

const SystemSetting = () => {
  const { t } = useTranslation();
  let [inputs, setInputs] = useState(() => ({ ...defaultInputs, EmailDomainWhitelist: [] }));
  const [originInputs, setOriginInputs] = useState({});
  const [secretStates, setSecretStates] = useState(() => createInitialSecretStates(SYSTEM_SECRET_OPTION_KEYS));
  let [loading, setLoading] = useState(false);
  const [EmailDomainWhitelist, setEmailDomainWhitelist] = useState([]);
  const [showPasswordWarningModal, setShowPasswordWarningModal] = useState(false);
  const loadStatus = useContext(LoadStatusContext);

  const getOptions = async () => {
    try {
      const res = await API.get('/api/option/');
      const { success, message, data, meta } = res.data;
      if (success) {
        let newInputs = { ...defaultInputs };
        data.forEach((item) => {
          newInputs[item.key] = item.value;
        });
        const emailDomains = (newInputs.EmailDomainWhitelist || '').split(',');
        setInputs({
          ...newInputs,
          EmailDomainWhitelist: emailDomains
        });
        setOriginInputs(newInputs);
        setSecretStates(mergeSecretStatesFromMeta(SYSTEM_SECRET_OPTION_KEYS, meta?.sensitive_options));

        setEmailDomainWhitelist(emailDomains);
      } else {
        showError(message);
      }
    } catch (error) {
      return;
    }
  };

  useEffect(() => {
    getOptions().then();
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

  const refreshSettings = async () => {
    await getOptions();
    await loadStatus();
  };

  const updateOption = async (key, value) => {
    setLoading(true);
    switch (key) {
      case 'PasswordLoginEnabled':
      case 'PasswordRegisterEnabled':
      case 'EmailVerificationEnabled':
      case 'GitHubOldIdCloseEnabled':
      case 'RegisterEnabled':
        value = inputs[key] === 'true' ? 'false' : 'true';
        break;
      default:
        break;
    }

    try {
      const res = await API.put('/api/option/', {
        key,
        value: normalizeOptionPayloadValue(value)
      });
      const { success, message } = res.data;
      if (success) {
        if (key === 'EmailDomainWhitelist') {
          value = value.split(',');
        }
        setInputs((inputs) => ({
          ...inputs,
          [key]: value
        }));
        await refreshSettings();
        showSuccess('设置成功！');
      } else {
        showError(message);
      }
    } catch (error) {
      showError(getRequestErrorMessage(error, '设置失败'));
    } finally {
      setLoading(false);
    }
  };

  const putOptionBatchOrThrow = async (updates) => {
    try {
      const res = await API.put('/api/option/batch', { updates });
      const { success, message } = res.data;
      if (!success) {
        throw new Error(message || '设置失败');
      }
    } catch (error) {
      throw new Error(getRequestErrorMessage(error, '设置失败'));
    }
  };

  const buildBatchUpdates = (keys, options = {}, sourceInputs = inputs) => {
    const { transformers = {} } = options;

    return keys.flatMap((key) => {
      const transform = transformers[key];
      const nextValue = transform ? transform(sourceInputs[key]) : sourceInputs[key];

      if (originInputs[key] === nextValue) {
        return [];
      }

      return [
        {
          key,
          value: normalizeOptionPayloadValue(nextValue)
        }
      ];
    });
  };

  const assertSecretUpdatesAllowed = (secretKeys = [], sourceInputs = inputs) => {
    for (const key of secretKeys) {
      const blockedMessage = getSecretClearBlockedMessage(key, sourceInputs, t);
      if (blockedMessage && buildSecretOptionUpdates([key], secretStates).some((update) => update.value === '')) {
        throw new Error(blockedMessage);
      }
    }
  };

  const submitPreparedUpdates = async (updates, options = {}) => {
    const { secretKeys = [], sourceInputs = inputs } = options;

    setLoading(true);
    try {
      assertSecretUpdatesAllowed(secretKeys, sourceInputs);
      if (updates.length > 0) {
        await putOptionBatchOrThrow(updates);
      }
      await refreshSettings();
      showSuccess('设置成功！');
    } catch (error) {
      showError(error.message || '设置失败');
    } finally {
      setLoading(false);
    }
  };

  const submitOptionBatch = async (keys, options = {}, sourceInputs = inputs) => {
    const secretKeys = options.secretKeys || [];
    const regularKeys = keys.filter((key) => !secretKeys.includes(key));
    const updates = [...buildBatchUpdates(regularKeys, options, sourceInputs), ...buildSecretOptionUpdates(secretKeys, secretStates)];

    await submitPreparedUpdates(updates, { secretKeys, sourceInputs });
  };

  const submitProviderSection = async (toggleKey, sourceInputs = inputs) => {
    const sectionConfig = getSystemProviderSectionConfig(toggleKey);
    if (!sectionConfig) {
      showError('设置失败');
      return;
    }

    const updates = buildSystemProviderSectionUpdates(sectionConfig, {
      originInputs,
      sourceInputs,
      secretStates,
      normalizeOptionPayloadValue
    });

    await submitPreparedUpdates(updates, {
      secretKeys: sectionConfig.secretKeys,
      sourceInputs
    });
  };

  const deferredBooleanFields = new Set(['EmailDomainRestrictionEnabled', ...SYSTEM_PROVIDER_TOGGLE_KEYS]);

  const deferredFields = new Set([
    'Notice',
    'ServerAddress',
    'SMTPServer',
    'SMTPPort',
    'SMTPAccount',
    'SMTPFrom',
    'SMTPToken',
    'GitHubClientId',
    'GitHubClientSecret',
    'OIDCClientId',
    'OIDCClientSecret',
    'OIDCIssuer',
    'OIDCScopes',
    'OIDCUsernameClaims',
    'WeChatServerAddress',
    'WeChatServerToken',
    'WeChatAccountQRCodeImageURL',
    'TurnstileSiteKey',
    'TurnstileSecretKey',
    'EmailDomainWhitelist',
    'LarkClientId',
    'LarkClientSecret',
    ...deferredBooleanFields
  ]);

  const handleInputChange = async (event) => {
    let { name, value } = event.target;

    if (name === 'PasswordLoginEnabled' && inputs[name] === 'true') {
      // block disabling password login
      setShowPasswordWarningModal(true);
      return;
    }
    if (SYSTEM_SECRET_OPTION_KEYS.includes(name)) {
      setSecretStates((prev) => updateSecretOptionDraft(prev, name, value));
      return;
    }
    if (deferredFields.has(name)) {
      if (deferredBooleanFields.has(name)) {
        value = inputs[name] === 'true' ? 'false' : 'true';
      }
      setInputs((inputs) => ({ ...inputs, [name]: value }));
    } else {
      await updateOption(name, value);
    }
  };

  const submitServerAddress = async () => {
    await submitOptionBatch(['ServerAddress'], {
      transformers: {
        ServerAddress: (value) => removeTrailingSlash(value)
      }
    });
  };

  const submitSMTP = async () => {
    await submitOptionBatch(['SMTPServer', 'SMTPAccount', 'SMTPFrom', 'SMTPPort', 'SMTPToken'], {
      secretKeys: ['SMTPToken']
    });
  };

  const submitEmailDomainWhitelist = async () => {
    await submitOptionBatch(['EmailDomainRestrictionEnabled', 'EmailDomainWhitelist'], {
      transformers: {
        EmailDomainWhitelist: (value) => value.join(',')
      }
    });
  };

  const handleSecretClear = (key, sourceInputs = inputs) => {
    const blockedMessage = getSecretClearBlockedMessage(key, sourceInputs, t);
    if (blockedMessage) {
      showError(blockedMessage);
      return;
    }
    setSecretStates((prev) => markSecretOptionForClear(prev, key));
  };

  const handleSecretReset = (key) => {
    setSecretStates((prev) => resetSecretOptionAction(prev, key));
  };

  return (
    <>
      <Stack spacing={2}>
        <SubCard title={t('setting_index.systemSettings.generalSettings.title')}>
          <Grid container spacing={{ xs: 3, sm: 2, md: 4 }}>
            <Grid xs={12}>
              <FormControl fullWidth>
                <InputLabel htmlFor="ServerAddress">{t('setting_index.systemSettings.generalSettings.serverAddress')}</InputLabel>
                <OutlinedInput
                  id="ServerAddress"
                  name="ServerAddress"
                  value={inputs.ServerAddress || ''}
                  onChange={handleInputChange}
                  label={t('setting_index.systemSettings.generalSettings.serverAddress')}
                  placeholder={t('setting_index.systemSettings.generalSettings.serverAddressPlaceholder')}
                  disabled={loading}
                />
              </FormControl>
            </Grid>
            <Grid xs={12}>
              <Button variant="contained" onClick={submitServerAddress}>
                {t('setting_index.systemSettings.generalSettings.updateServerAddress')}
              </Button>
            </Grid>
          </Grid>
        </SubCard>

        <SubCard title={t('setting_index.systemSettings.configureLoginRegister.title')}>
          <Grid container spacing={{ xs: 3, sm: 2, md: 4 }}>
            <Grid xs={12} md={3}>
              <FormControlLabel
                label={t('setting_index.systemSettings.configureLoginRegister.passwordLogin')}
                control={
                  <Checkbox checked={inputs.PasswordLoginEnabled === 'true'} onChange={handleInputChange} name="PasswordLoginEnabled" />
                }
              />
            </Grid>
            <Grid xs={12} md={3}>
              <FormControlLabel
                label={t('setting_index.systemSettings.configureLoginRegister.passwordRegister')}
                control={
                  <Checkbox
                    checked={inputs.PasswordRegisterEnabled === 'true'}
                    onChange={handleInputChange}
                    name="PasswordRegisterEnabled"
                  />
                }
              />
            </Grid>
            <Grid xs={12} md={3}>
              <FormControlLabel
                label={t('setting_index.systemSettings.configureLoginRegister.emailVerification')}
                control={
                  <Checkbox
                    checked={inputs.EmailVerificationEnabled === 'true'}
                    onChange={handleInputChange}
                    name="EmailVerificationEnabled"
                  />
                }
              />
            </Grid>
            <Grid xs={12} md={3}>
              <FormControlLabel
                label={t('setting_index.systemSettings.configureLoginRegister.registerEnabled')}
                control={<Checkbox checked={inputs.RegisterEnabled === 'true'} onChange={handleInputChange} name="RegisterEnabled" />}
              />
            </Grid>

            <Grid xs={12} md={3}>
              <FormControlLabel
                label={t('setting_index.systemSettings.configureLoginRegister.gitHubOldIdClose')}
                control={
                  <Checkbox
                    checked={inputs.GitHubOldIdCloseEnabled === 'true'}
                    onChange={handleInputChange}
                    name="GitHubOldIdCloseEnabled"
                  />
                }
              />
            </Grid>
          </Grid>
        </SubCard>

        <SubCard
          title={t('setting_index.systemSettings.configureEmailDomainWhitelist.title')}
          subTitle={t('setting_index.systemSettings.configureEmailDomainWhitelist.subTitle')}
        >
          <Grid container spacing={{ xs: 3, sm: 2, md: 4 }}>
            <Grid xs={12}>
              <FormControlLabel
                label={t('setting_index.systemSettings.configureEmailDomainWhitelist.emailDomainRestriction')}
                control={
                  <Checkbox
                    checked={inputs.EmailDomainRestrictionEnabled === 'true'}
                    onChange={handleInputChange}
                    name="EmailDomainRestrictionEnabled"
                  />
                }
              />
            </Grid>
            <Grid xs={12}>
              <FormControl fullWidth>
                <Autocomplete
                  multiple
                  freeSolo
                  id="EmailDomainWhitelist"
                  options={EmailDomainWhitelist}
                  value={inputs.EmailDomainWhitelist}
                  onChange={(e, value) => {
                    const event = {
                      target: {
                        name: 'EmailDomainWhitelist',
                        value: value
                      }
                    };
                    handleInputChange(event);
                  }}
                  filterSelectedOptions
                  renderInput={(params) => (
                    <TextField
                      {...params}
                      name="EmailDomainWhitelist"
                      label={t('setting_index.systemSettings.configureEmailDomainWhitelist.allowedEmailDomains')}
                    />
                  )}
                  filterOptions={(options, params) => {
                    const filtered = filter(options, params);
                    const { inputValue } = params;
                    const isExisting = options.some((option) => inputValue === option);
                    if (inputValue !== '' && !isExisting) {
                      filtered.push(inputValue);
                    }
                    return filtered;
                  }}
                />
              </FormControl>
            </Grid>
            <Grid xs={12}>
              <Button variant="contained" onClick={submitEmailDomainWhitelist}>
                {t('setting_index.systemSettings.configureEmailDomainWhitelist.save')}
              </Button>
            </Grid>
          </Grid>
        </SubCard>

        <SubCard
          title={t('setting_index.systemSettings.configureSMTP.title')}
          subTitle={t('setting_index.systemSettings.configureSMTP.subTitle')}
        >
          <Grid container spacing={{ xs: 3, sm: 2, md: 4 }}>
            <Grid xs={12}>
              <Alert severity="info">{t('setting_index.systemSettings.configureSMTP.alert')}</Alert>
            </Grid>
            <Grid xs={12} md={4}>
              <FormControl fullWidth>
                <InputLabel htmlFor="SMTPServer">{t('setting_index.systemSettings.configureSMTP.smtpServer')}</InputLabel>
                <OutlinedInput
                  id="SMTPServer"
                  name="SMTPServer"
                  value={inputs.SMTPServer || ''}
                  onChange={handleInputChange}
                  label={t('setting_index.systemSettings.configureSMTP.smtpServer')}
                  placeholder={t('setting_index.systemSettings.configureSMTP.smtpServerPlaceholder')}
                  disabled={loading}
                />
              </FormControl>
            </Grid>
            <Grid xs={12} md={4}>
              <FormControl fullWidth>
                <InputLabel htmlFor="SMTPPort">{t('setting_index.systemSettings.configureSMTP.smtpPort')}</InputLabel>
                <OutlinedInput
                  id="SMTPPort"
                  name="SMTPPort"
                  value={inputs.SMTPPort || ''}
                  onChange={handleInputChange}
                  label={t('setting_index.systemSettings.configureSMTP.smtpPort')}
                  placeholder={t('setting_index.systemSettings.configureSMTP.smtpPortPlaceholder')}
                  disabled={loading}
                />
              </FormControl>
            </Grid>
            <Grid xs={12} md={4}>
              <FormControl fullWidth>
                <InputLabel htmlFor="SMTPAccount">{t('setting_index.systemSettings.configureSMTP.smtpAccount')}</InputLabel>
                <OutlinedInput
                  id="SMTPAccount"
                  name="SMTPAccount"
                  value={inputs.SMTPAccount || ''}
                  onChange={handleInputChange}
                  label={t('setting_index.systemSettings.configureSMTP.smtpAccount')}
                  placeholder={t('setting_index.systemSettings.configureSMTP.smtpAccountPlaceholder')}
                  disabled={loading}
                />
              </FormControl>
            </Grid>
            <Grid xs={12} md={4}>
              <FormControl fullWidth>
                <InputLabel htmlFor="SMTPFrom">{t('setting_index.systemSettings.configureSMTP.smtpFrom')}</InputLabel>
                <OutlinedInput
                  id="SMTPFrom"
                  name="SMTPFrom"
                  value={inputs.SMTPFrom || ''}
                  onChange={handleInputChange}
                  label={t('setting_index.systemSettings.configureSMTP.smtpFrom')}
                  placeholder={t('setting_index.systemSettings.configureSMTP.smtpFromPlaceholder')}
                  disabled={loading}
                />
              </FormControl>
            </Grid>
            <Grid xs={12} md={4}>
              <SecretOptionField
                id="SMTPToken"
                name="SMTPToken"
                label={t('setting_index.systemSettings.configureSMTP.smtpToken')}
                placeholder={t('setting_index.systemSettings.configureSMTP.smtpTokenPlaceholder')}
                secretState={secretStates.SMTPToken}
                onChange={handleInputChange}
                onClear={() => handleSecretClear('SMTPToken')}
                onReset={() => handleSecretReset('SMTPToken')}
                disabled={loading}
                t={t}
              />
            </Grid>
            <Grid xs={12}>
              <Button variant="contained" onClick={submitSMTP}>
                {t('setting_index.systemSettings.configureSMTP.save')}
              </Button>
            </Grid>
          </Grid>
        </SubCard>

        <SubCard
          title={t('setting_index.systemSettings.configureGitHubOAuthApp.title')}
          subTitle={
            <span>
              {' '}
              {t('setting_index.systemSettings.configureGitHubOAuthApp.subTitle')}
              <a href="https://github.com/settings/developers" target="_blank" rel="noopener noreferrer">
                {t('setting_index.systemSettings.configureGitHubOAuthApp.manageLink')}
              </a>
              {t('setting_index.systemSettings.configureGitHubOAuthApp.manage')}
            </span>
          }
        >
          <Grid container spacing={{ xs: 3, sm: 2, md: 4 }}>
            <Grid xs={12}>
              <FormControlLabel
                label={t('setting_index.systemSettings.configureLoginRegister.gitHubOAuth')}
                control={<Checkbox checked={inputs.GitHubOAuthEnabled === 'true'} onChange={handleInputChange} name="GitHubOAuthEnabled" />}
              />
            </Grid>
            <Grid xs={12}>
              <Alert severity="info" sx={{ wordWrap: 'break-word' }}>
                {t('setting_index.systemSettings.configureGitHubOAuthApp.alert1')} <b>{inputs.ServerAddress}</b>
                {t('setting_index.systemSettings.configureGitHubOAuthApp.alert2')} <b>{`${inputs.ServerAddress}/oauth/github`}</b>
              </Alert>
            </Grid>
            <Grid xs={12} md={6}>
              <FormControl fullWidth>
                <InputLabel htmlFor="GitHubClientId">{t('setting_index.systemSettings.configureGitHubOAuthApp.clientId')}</InputLabel>
                <OutlinedInput
                  id="GitHubClientId"
                  name="GitHubClientId"
                  value={inputs.GitHubClientId || ''}
                  onChange={handleInputChange}
                  label={t('setting_index.systemSettings.configureGitHubOAuthApp.clientId')}
                  placeholder={t('setting_index.systemSettings.configureGitHubOAuthApp.clientIdPlaceholder')}
                  disabled={loading}
                />
              </FormControl>
            </Grid>
            <Grid xs={12} md={6}>
              <SecretOptionField
                id="GitHubClientSecret"
                name="GitHubClientSecret"
                label={t('setting_index.systemSettings.configureGitHubOAuthApp.clientSecret')}
                placeholder={t('setting_index.systemSettings.configureGitHubOAuthApp.clientSecretPlaceholder')}
                secretState={secretStates.GitHubClientSecret}
                onChange={handleInputChange}
                onClear={() => handleSecretClear('GitHubClientSecret')}
                onReset={() => handleSecretReset('GitHubClientSecret')}
                disabled={loading}
                t={t}
              />
            </Grid>
            <Grid xs={12}>
              <Button variant="contained" onClick={() => submitProviderSection('GitHubOAuthEnabled')}>
                {t('setting_index.systemSettings.configureGitHubOAuthApp.saveButton')}
              </Button>
            </Grid>
          </Grid>
        </SubCard>

        <SubCard
          title={t('setting_index.systemSettings.configureWeChatServer.title')}
          subTitle={
            <span>
              {t('setting_index.systemSettings.configureWeChatServer.subTitle')}
              <a href="https://github.com/songquanpeng/wechat-server" target="_blank" rel="noopener noreferrer">
                {t('setting_index.systemSettings.configureWeChatServer.learnLink')}
              </a>
              {t('setting_index.systemSettings.configureWeChatServer.learn')}
            </span>
          }
        >
          <Grid container spacing={{ xs: 3, sm: 2, md: 4 }}>
            <Grid xs={12}>
              <FormControlLabel
                label={t('setting_index.systemSettings.configureLoginRegister.weChatAuth')}
                control={<Checkbox checked={inputs.WeChatAuthEnabled === 'true'} onChange={handleInputChange} name="WeChatAuthEnabled" />}
              />
            </Grid>
            <Grid xs={12} md={4}>
              <FormControl fullWidth>
                <InputLabel htmlFor="WeChatServerAddress">
                  {t('setting_index.systemSettings.configureWeChatServer.serverAddress')}
                </InputLabel>
                <OutlinedInput
                  id="WeChatServerAddress"
                  name="WeChatServerAddress"
                  value={inputs.WeChatServerAddress || ''}
                  onChange={handleInputChange}
                  label={t('setting_index.systemSettings.configureWeChatServer.serverAddress')}
                  placeholder={t('setting_index.systemSettings.configureWeChatServer.serverAddressPlaceholder')}
                  disabled={loading}
                />
              </FormControl>
            </Grid>
            <Grid xs={12} md={4}>
              <SecretOptionField
                id="WeChatServerToken"
                name="WeChatServerToken"
                label={t('setting_index.systemSettings.configureWeChatServer.accessToken')}
                placeholder={t('setting_index.systemSettings.configureWeChatServer.accessTokenPlaceholder')}
                secretState={secretStates.WeChatServerToken}
                onChange={handleInputChange}
                onClear={() => handleSecretClear('WeChatServerToken')}
                onReset={() => handleSecretReset('WeChatServerToken')}
                disabled={loading}
                t={t}
              />
            </Grid>
            <Grid xs={12} md={4}>
              <FormControl fullWidth>
                <InputLabel htmlFor="WeChatAccountQRCodeImageURL">
                  {t('setting_index.systemSettings.configureWeChatServer.qrCodeImage')}
                </InputLabel>
                <OutlinedInput
                  id="WeChatAccountQRCodeImageURL"
                  name="WeChatAccountQRCodeImageURL"
                  value={inputs.WeChatAccountQRCodeImageURL || ''}
                  onChange={handleInputChange}
                  label={t('setting_index.systemSettings.configureWeChatServer.qrCodeImage')}
                  placeholder={t('setting_index.systemSettings.configureWeChatServer.qrCodeImagePlaceholder')}
                  disabled={loading}
                />
              </FormControl>
            </Grid>
            <Grid xs={12}>
              <Button variant="contained" onClick={() => submitProviderSection('WeChatAuthEnabled')}>
                {t('setting_index.systemSettings.configureWeChatServer.saveButton')}
              </Button>
            </Grid>
          </Grid>
        </SubCard>

        <SubCard
          title={t('setting_index.systemSettings.configureFeishuAuthorization.title')}
          subTitle={
            <span>
              {' '}
              {t('setting_index.systemSettings.configureFeishuAuthorization.subTitle')}
              <a href="https://open.feishu.cn/app" target="_blank" rel="noreferrer">
                {t('setting_index.systemSettings.configureFeishuAuthorization.manageLink')}
              </a>
              {t('setting_index.systemSettings.configureFeishuAuthorization.manage')}
            </span>
          }
        >
          <Grid container spacing={{ xs: 3, sm: 2, md: 4 }}>
            <Grid xs={12}>
              <FormControlLabel
                label={t('setting_index.systemSettings.configureLoginRegister.larkAuth')}
                control={<Checkbox checked={inputs.LarkAuthEnabled === 'true'} onChange={handleInputChange} name="LarkAuthEnabled" />}
              />
            </Grid>
            <Grid xs={12}>
              <Alert severity="info" sx={{ wordWrap: 'break-word' }}>
                {t('setting_index.systemSettings.configureFeishuAuthorization.alert1')} <code>{inputs.ServerAddress}</code>
                {t('setting_index.systemSettings.configureFeishuAuthorization.alert2')} <code>{`${inputs.ServerAddress}/oauth/lark`}</code>
              </Alert>
            </Grid>
            <Grid xs={12} md={6}>
              <FormControl fullWidth>
                <InputLabel htmlFor="LarkClientId">{t('setting_index.systemSettings.configureFeishuAuthorization.appId')}</InputLabel>
                <OutlinedInput
                  id="LarkClientId"
                  name="LarkClientId"
                  value={inputs.LarkClientId || ''}
                  onChange={handleInputChange}
                  label={t('setting_index.systemSettings.configureFeishuAuthorization.appId')}
                  placeholder={t('setting_index.systemSettings.configureFeishuAuthorization.appIdPlaceholder')}
                  disabled={loading}
                />
              </FormControl>
            </Grid>
            <Grid xs={12} md={6}>
              <SecretOptionField
                id="LarkClientSecret"
                name="LarkClientSecret"
                label={t('setting_index.systemSettings.configureFeishuAuthorization.appSecret')}
                placeholder={t('setting_index.systemSettings.configureFeishuAuthorization.appSecretPlaceholder')}
                secretState={secretStates.LarkClientSecret}
                onChange={handleInputChange}
                onClear={() => handleSecretClear('LarkClientSecret')}
                onReset={() => handleSecretReset('LarkClientSecret')}
                disabled={loading}
                t={t}
              />
            </Grid>
            <Grid xs={12}>
              <Button variant="contained" onClick={() => submitProviderSection('LarkAuthEnabled')}>
                {t('setting_index.systemSettings.configureFeishuAuthorization.saveButton')}
              </Button>
            </Grid>
          </Grid>
        </SubCard>

        <SubCard
          title={t('setting_index.systemSettings.configureTurnstile.title')}
          subTitle={
            <span>
              {t('setting_index.systemSettings.configureTurnstile.subTitle')}
              <a href="https://dash.cloudflare.com/" target="_blank" rel="noopener noreferrer">
                {t('setting_index.systemSettings.configureTurnstile.manageLink')}
              </a>
              {t('setting_index.systemSettings.configureTurnstile.manage')}
            </span>
          }
        >
          <Grid container spacing={{ xs: 3, sm: 2, md: 4 }}>
            <Grid xs={12}>
              <FormControlLabel
                label={t('setting_index.systemSettings.configureLoginRegister.turnstileCheck')}
                control={
                  <Checkbox checked={inputs.TurnstileCheckEnabled === 'true'} onChange={handleInputChange} name="TurnstileCheckEnabled" />
                }
              />
            </Grid>
            <Grid xs={12} md={6}>
              <FormControl fullWidth>
                <InputLabel htmlFor="TurnstileSiteKey">{t('setting_index.systemSettings.configureTurnstile.siteKey')}</InputLabel>
                <OutlinedInput
                  id="TurnstileSiteKey"
                  name="TurnstileSiteKey"
                  value={inputs.TurnstileSiteKey || ''}
                  onChange={handleInputChange}
                  label={t('setting_index.systemSettings.configureTurnstile.siteKey')}
                  placeholder={t('setting_index.systemSettings.configureTurnstile.siteKeyPlaceholder')}
                  disabled={loading}
                />
              </FormControl>
            </Grid>
            <Grid xs={12} md={6}>
              <SecretOptionField
                id="TurnstileSecretKey"
                name="TurnstileSecretKey"
                label={t('setting_index.systemSettings.configureTurnstile.secretKey')}
                placeholder={t('setting_index.systemSettings.configureTurnstile.secretKeyPlaceholder')}
                secretState={secretStates.TurnstileSecretKey}
                onChange={handleInputChange}
                onClear={() => handleSecretClear('TurnstileSecretKey')}
                onReset={() => handleSecretReset('TurnstileSecretKey')}
                disabled={loading}
                t={t}
              />
            </Grid>
            <Grid xs={12}>
              <Button variant="contained" onClick={() => submitProviderSection('TurnstileCheckEnabled')}>
                {t('setting_index.systemSettings.configureTurnstile.saveButton')}
              </Button>
            </Grid>
          </Grid>
        </SubCard>

        <SubCard
          title={t('setting_index.systemSettings.configureOIDCAuthorization.title')}
          subTitle={<span>{t('setting_index.systemSettings.configureOIDCAuthorization.subTitle')}</span>}
        >
          <Grid container spacing={{ xs: 3, sm: 2, md: 4 }}>
            <Grid xs={12}>
              <FormControlLabel
                label={t('setting_index.systemSettings.configureLoginRegister.oidcAuth')}
                control={<Checkbox checked={inputs.OIDCAuthEnabled === 'true'} onChange={handleInputChange} name="OIDCAuthEnabled" />}
              />
            </Grid>
            <Grid xs={12}>
              <Alert severity="info" sx={{ wordWrap: 'break-word' }}>
                {t('setting_index.systemSettings.configureOIDCAuthorization.alert1')} <b>{inputs.ServerAddress}</b>
                {t('setting_index.systemSettings.configureOIDCAuthorization.alert2')} <b>{`${inputs.ServerAddress}/oauth/oidc`}</b>
              </Alert>
            </Grid>

            <Grid xs={12} md={6}>
              <FormControl fullWidth>
                <InputLabel htmlFor="OIDCClientId">{t('setting_index.systemSettings.configureOIDCAuthorization.clientId')}</InputLabel>
                <OutlinedInput
                  id="OIDCClientId"
                  name="OIDCClientId"
                  value={inputs.OIDCClientId || ''}
                  onChange={handleInputChange}
                  label={t('setting_index.systemSettings.configureOIDCAuthorization.clientId')}
                  placeholder={t('setting_index.systemSettings.configureOIDCAuthorization.clientIdPlaceholder')}
                  disabled={loading}
                />
              </FormControl>
            </Grid>

            <Grid xs={12} md={6}>
              <SecretOptionField
                id="OIDCClientSecret"
                name="OIDCClientSecret"
                label={t('setting_index.systemSettings.configureOIDCAuthorization.clientSecret')}
                placeholder={t('setting_index.systemSettings.configureOIDCAuthorization.clientSecretPlaceholder')}
                secretState={secretStates.OIDCClientSecret}
                onChange={handleInputChange}
                onClear={() => handleSecretClear('OIDCClientSecret')}
                onReset={() => handleSecretReset('OIDCClientSecret')}
                disabled={loading}
                t={t}
              />
            </Grid>

            <Grid xs={12} md={6}>
              <FormControl fullWidth>
                <InputLabel htmlFor="OIDCIssuer">{t('setting_index.systemSettings.configureOIDCAuthorization.issuer')}</InputLabel>
                <OutlinedInput
                  id="OIDCIssuer"
                  name="OIDCIssuer"
                  value={inputs.OIDCIssuer || ''}
                  onChange={handleInputChange}
                  label={t('setting_index.systemSettings.configureOIDCAuthorization.issuer')}
                  placeholder={t('setting_index.systemSettings.configureOIDCAuthorization.issuerPlaceholder')}
                  disabled={loading}
                />
              </FormControl>
            </Grid>

            <Grid xs={12} md={6}>
              <FormControl fullWidth>
                <InputLabel htmlFor="OIDCScopes">{t('setting_index.systemSettings.configureOIDCAuthorization.scopes')}</InputLabel>
                <OutlinedInput
                  id="OIDCScopes"
                  name="OIDCScopes"
                  value={inputs.OIDCScopes || ''}
                  onChange={handleInputChange}
                  label={t('setting_index.systemSettings.configureOIDCAuthorization.scopes')}
                  placeholder={t('setting_index.systemSettings.configureOIDCAuthorization.scopesPlaceholder')}
                  disabled={loading}
                />
              </FormControl>
            </Grid>

            <Grid xs={12} md={6}>
              <FormControl fullWidth>
                <InputLabel htmlFor="OIDCUsernameClaims">
                  {t('setting_index.systemSettings.configureOIDCAuthorization.usernameClaims')}
                </InputLabel>
                <OutlinedInput
                  id="OIDCUsernameClaims"
                  name="OIDCUsernameClaims"
                  value={inputs.OIDCUsernameClaims || ''}
                  onChange={handleInputChange}
                  label={t('setting_index.systemSettings.configureOIDCAuthorization.usernameClaims')}
                  placeholder={t('setting_index.systemSettings.configureOIDCAuthorization.usernameClaimsPlaceholder')}
                  disabled={loading}
                />
              </FormControl>
            </Grid>

            <Grid xs={12}>
              <Button variant="contained" onClick={() => submitProviderSection('OIDCAuthEnabled')}>
                {t('setting_index.systemSettings.configureOIDCAuthorization.saveButton')}
              </Button>
            </Grid>
          </Grid>
        </SubCard>
      </Stack>
      <Dialog open={showPasswordWarningModal} onClose={() => setShowPasswordWarningModal(false)} maxWidth={'md'}>
        <DialogTitle sx={{ margin: '0px', fontWeight: 700, lineHeight: '1.55556', padding: '24px', fontSize: '1.125rem' }}>
          警告
        </DialogTitle>
        <Divider />
        <DialogContent>取消密码登录将导致所有未绑定其他登录方式的用户（包括管理员）无法通过密码登录，确认取消？</DialogContent>
        <DialogActions>
          <Button onClick={() => setShowPasswordWarningModal(false)}>取消</Button>
          <Button
            sx={{ color: 'error.main' }}
            onClick={async () => {
              setShowPasswordWarningModal(false);
              await updateOption('PasswordLoginEnabled', 'false');
            }}
          >
            确定
          </Button>
        </DialogActions>
      </Dialog>
    </>
  );
};

export default SystemSetting;
