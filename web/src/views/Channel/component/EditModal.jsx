import PropTypes from 'prop-types';
import { memo, useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { CHANNEL_OPTIONS } from 'constants/ChannelConstants';
import { useTheme } from '@mui/material/styles';
import { API } from 'utils/api';
import { showError, showSuccess, trims, copy } from 'utils/common';
import {
  Dialog,
  DialogTitle,
  DialogContent,
  DialogActions,
  TextField,
  Button,
  Divider,
  Select,
  MenuItem,
  FormControl,
  InputLabel,
  OutlinedInput,
  InputAdornment,
  ButtonGroup,
  Container,
  Autocomplete,
  FormHelperText,
  Switch,
  FormControlLabel,
  Typography,
  Tooltip,
  IconButton,
  Collapse,
  Box,
  useMediaQuery
} from '@mui/material';
import { useField } from 'formik';
import * as Yup from 'yup';
import { defaultConfig, typeConfig } from '../type/Config'; //typeConfig
import { useTranslation } from 'react-i18next';
import useCustomizeT from 'hooks/useCustomizeT';
import { PreCostType, isSelfHostedResponsesWSEnabled, normalizeChannelOtherForRequest } from '../type/other';
import MapInput from './MapInput';
import ListInput from './ListInput';
import ModelSelectorModal from './ModelSelectorModal';
import ChannelModelsField from './ChannelModelsField';
import ChannelForm from './ChannelForm';
import pluginList from '../type/Plugin.json';
import { Icon } from '@iconify/react';
import Editor from '@monaco-editor/react';
import CodexAuthControls from './CodexAuthControls';
import ConfirmDialog from 'ui-component/confirm-dialog';
import ChannelEndpointsEditor from './ChannelEndpointsEditor';
import { createEndpointPreset } from '../type/endpoints.mjs';

const isAzureV1ResourceLevelBaseUrl = (value) => {
  const raw = String(value ?? '').trim();
  if (raw === '') {
    return false;
  }
  try {
    const parsed = new URL(raw);
    if (!['http:', 'https:'].includes(parsed.protocol)) {
      return false;
    }
    const segments = parsed.pathname
      .toLowerCase()
      .split('/')
      .filter((segment) => segment !== '');
    return !segments.some((segment, index) => segment === 'openai' && segments[index + 1] === 'deployments');
  } catch {
    return false;
  }
};

const getValidationSchema = (t) =>
  Yup.object().shape({
    is_edit: Yup.boolean(),
    // is_tag: Yup.boolean(),
    name: Yup.string().required(t('channel_edit.requiredName')),
    type: Yup.number().required(t('channel_edit.requiredChannel')),
    key: Yup.string().when('is_edit', { is: false, then: Yup.string().required(t('channel_edit.requiredKey')) }),
    other: Yup.string().test('azure-speech-region-or-base-url', t('channel_edit.requiredAzureSpeechRegionOrBaseUrl'), function (value) {
      if (Number(this.parent.type) !== 24 || String(this.parent.base_url ?? '').trim() !== '') {
        return true;
      }
      const raw = String(value ?? '').trim();
      if (raw === '') {
        return false;
      }
      try {
        const parsed = JSON.parse(raw);
        return Boolean(parsed && typeof parsed === 'object' && !Array.isArray(parsed) && String(parsed.region ?? '').trim() !== '');
      } catch {
        return false;
      }
    }),
    proxy: Yup.string(),
    test_model: Yup.string(),
    models: Yup.array().min(1, t('channel_edit.requiredModels')),
    groups: Yup.array().min(1, t('channel_edit.requiredGroup')),
    base_url: Yup.string().when('type', {
      is: (value) => [3, 8, 55].includes(value),
      then: Yup.string()
        .required(t('channel_edit.requiredBaseUrl'))
        .test('azure-v1-resource-level-base-url', t('channel_edit.invalidAzureV1ResourceBaseUrl'), function (value) {
          if (Number(this.parent.type) !== 55) {
            return true;
          }
          return isAzureV1ResourceLevelBaseUrl(value);
        }),
      otherwise: Yup.string() // 在其他情况下，base_url 可以是任意字符串
    }),
    model_mapping: Yup.array(),
    model_headers: Yup.array(),
    custom_parameter: Yup.string().nullable()
  });

const CUSTOM_PARAMETER_EDITOR_OPTIONS = {
  minimap: { enabled: false },
  scrollBeyondLastLine: false,
  automaticLayout: true,
  fontSize: 14,
  lineNumbers: 'on',
  folding: true,
  formatOnPaste: true,
  formatOnType: true
};

// 独立订阅 custom_parameter 字段，其它字段变化时 Monaco 不会收到新的 value/onChange。
const CustomParameterEditor = memo(function CustomParameterEditor() {
  const theme = useTheme();
  const [field, , helpers] = useField('custom_parameter');
  const handleChange = useCallback((value) => helpers.setValue(value), [helpers]);

  return (
    <Editor
      height="100%"
      language="json"
      theme={theme.palette.mode === 'dark' ? 'vs-dark' : 'light'}
      value={field.value}
      options={CUSTOM_PARAMETER_EDITOR_OPTIONS}
      onChange={handleChange}
    />
  );
});

const EditModal = ({ open, channelId, onCancel, onOk, groupOptions, isTag, modelOptions, prices }) => {
  const { t } = useTranslation();
  const { t: customizeT } = useCustomizeT();
  const theme = useTheme();
  const validationSchema = useMemo(() => getValidationSchema(t), [t]);
  const isMobile = useMediaQuery(theme.breakpoints.down('sm'));
  // const [loading, setLoading] = useState(false);
  const [initialInput, setInitialInput] = useState(defaultConfig.input);
  const [editConfirmationOpen, setEditConfirmationOpen] = useState(false);
  const editConfirmationResolver = useRef(null);
  const editSession = useRef(0);

  const resolveEditConfirmation = (confirmed) => {
    const resolve = editConfirmationResolver.current;
    editConfirmationResolver.current = null;
    setEditConfirmationOpen(false);
    resolve?.(confirmed);
  };

  useEffect(() => {
    setEditConfirmationOpen(false);
    return () => {
      editSession.current += 1;
      editConfirmationResolver.current?.(false);
      editConfirmationResolver.current = null;
    };
  }, [open, channelId]);
  const [inputLabel, setInputLabel] = useState(defaultConfig.inputLabel); //
  const [inputPrompt, setInputPrompt] = useState(defaultConfig.prompt);
  const [batchAdd, setBatchAdd] = useState(false);
  const [hasTag, setHasTag] = useState(false);
  const [expanded, setExpanded] = useState(false);
  const [endpointDefinitions, setEndpointDefinitions] = useState(null);
  const [endpointLoadError, setEndpointLoadError] = useState(false);
  useEffect(() => {
    if (!open) return;
    let cancelled = false;
    setEndpointLoadError(false);
    setEndpointDefinitions(null);
    API.get('/api/channel/endpoints')
      .then(({ data }) => {
        if (cancelled) return;
        if (!data.success || !Array.isArray(data.data)) throw new Error('invalid endpoint catalog');
        setEndpointDefinitions(data.data);
      })
      .catch(() => {
        if (!cancelled) setEndpointLoadError(true);
      });
    return () => {
      cancelled = true;
    };
  }, [open]);

  const [batchFileImporting, setBatchFileImporting] = useState(false);
  const [codexBatchAuthFileImporting, setCodexBatchAuthFileImporting] = useState(false);
  const removeDuplicates = (array) => [...new Set(array)];
  const [modelSelectorOpen, setModelSelectorOpen] = useState(false);
  const [tempFormikValues, setTempFormikValues] = useState(null);
  const [tempSetFieldValue, setTempSetFieldValue] = useState(null);
  const batchFileInputRef = useRef(null);
  const codexBatchAuthFileInputRef = useRef(null);
  const [otherConfigHelpOpen, setOtherConfigHelpOpen] = useState(false);
  const codexConfigHelpKey = 'channel_edit.codexConfigHelp';
  const otherConfigHelpKey = 'channel_edit.otherConfigHelp';
  const codexConfigFields = [
    [
      'execution_session_ttl_seconds',
      '600',
      t(`${codexConfigHelpKey}.positiveIntegerSeconds`),
      t(`${codexConfigHelpKey}.fields.executionSessionTTL`)
    ],
    ['self_hosted', 'false', 'true / false', t(`${codexConfigHelpKey}.fields.selfHosted`)],
    ['responses_ws_self_hosted', 'false', 'true / false', t('channel_edit.responsesWSSelfHostedHelp')]
  ];
  const responsesWSNativeExample = {
    title: t('channel_edit.responsesWSNative'),
    value: `{
  "responses_ws_native": true
}`
  };
  const responsesWSSelfHostedExample = {
    title: t('channel_edit.responsesWSSelfHosted'),
    value: `{
  "responses_ws_self_hosted": true
}`
  };
  const codexConfigExamples = [
    {
      title: t(`${codexConfigHelpKey}.examples.selfHosted`),
      value: `{
  "self_hosted": true
}`
    },
    responsesWSSelfHostedExample,
    {
      title: t(`${codexConfigHelpKey}.examples.sessionTTL`),
      value: `{
  "execution_session_ttl_seconds": 600
}`
    }
  ];
  const commonOtherConfigFields = {
    responses_ws_native: ['responses_ws_native', 'false', 'true / false', t('channel_edit.responsesWSNativeHelp')],
    responses_ws_self_hosted: ['responses_ws_self_hosted', 'false', 'true / false', t('channel_edit.responsesWSSelfHostedHelp')],
    self_hosted: ['self_hosted', 'false', 'true / false', t(`${otherConfigHelpKey}.fields.selfHosted`)],
    extra: ['extra', '{}', 'JSON object', t(`${otherConfigHelpKey}.fields.extra`)],
    vendor_extra: ['vendor_extra', '{}', 'JSON object', t(`${otherConfigHelpKey}.fields.vendorExtra`)]
  };
  const commonOpaqueFieldKeys = ['extra', 'vendor_extra'];
  const providerOtherConfigHelp = {
    1: {
      commonFieldKeys: ['responses_ws_native', 'responses_ws_self_hosted', 'self_hosted'],
      examples: [responsesWSNativeExample, responsesWSSelfHostedExample]
    },
    8: {
      commonFieldKeys: ['responses_ws_native', 'responses_ws_self_hosted', 'self_hosted'],
      examples: [responsesWSNativeExample, responsesWSSelfHostedExample]
    },
    3: {
      providerFields: [
        ['api_version', t(`${otherConfigHelpKey}.required`), 'non-empty string', t(`${otherConfigHelpKey}.fields.azureAPIVersion`)]
      ],
      commonFieldKeys: ['responses_ws_self_hosted', 'self_hosted'],
      examples: [
        {
          title: t(`${otherConfigHelpKey}.examples.azureClassic`),
          value: `{
  "api_version": "2024-05-01-preview"
}`
        },
        {
          title: t('channel_edit.responsesWSSelfHosted'),
          value: `{
  "api_version": "2024-05-01-preview",
  "responses_ws_self_hosted": true
}`
        }
      ]
    },
    55: {
      commonFieldKeys: ['responses_ws_self_hosted', 'self_hosted'],
      examples: [responsesWSSelfHostedExample]
    },
    17: {
      providerFields: [['dashscope_plugin', t(`${otherConfigHelpKey}.empty`), 'string', t(`${otherConfigHelpKey}.fields.dashscopePlugin`)]],
      examples: [
        {
          title: t(`${otherConfigHelpKey}.examples.ali`),
          value: `{
  "dashscope_plugin": "plugin-name"
}`
        }
      ]
    },
    18: {
      providerFields: [['api_version', t(`${otherConfigHelpKey}.empty`), 'string', t(`${otherConfigHelpKey}.fields.xunfeiAPIVersion`)]],
      examples: [
        {
          title: t(`${otherConfigHelpKey}.examples.xunfei`),
          value: `{
  "api_version": "v3.1"
}`
        }
      ]
    },
    25: {
      providerFields: [['api_version', t(`${otherConfigHelpKey}.empty`), 'string', t(`${otherConfigHelpKey}.fields.geminiAPIVersion`)]],
      examples: [
        {
          title: t(`${otherConfigHelpKey}.examples.gemini`),
          value: `{
  "api_version": "v1"
}`
        }
      ]
    },
    24: {
      providerFields: [['region', t(`${otherConfigHelpKey}.empty`), 'string', t(`${otherConfigHelpKey}.fields.azureSpeechRegion`)]],
      examples: [
        {
          title: t(`${otherConfigHelpKey}.examples.azureSpeech`),
          value: `{
  "region": "eastasia"
}`
        }
      ]
    },
    42: {
      providerFields: [
        ['region', t(`${otherConfigHelpKey}.required`), 'string', t(`${otherConfigHelpKey}.fields.vertexRegion`)],
        ['project_id', t(`${otherConfigHelpKey}.required`), 'string', t(`${otherConfigHelpKey}.fields.vertexProjectID`)]
      ],
      examples: [
        {
          title: t(`${otherConfigHelpKey}.examples.vertex`),
          value: `{
  "region": "us-central1",
  "project_id": "my-project"
}`
        }
      ]
    }
  };
  const buildOtherConfigHelp = (channelType) => {
    if (channelType === 101) {
      return {
        title: t(`${codexConfigHelpKey}.title`),
        intro: t(`${codexConfigHelpKey}.intro`),
        sections: [{ title: t(`${codexConfigHelpKey}.fieldsTitle`), fields: codexConfigFields }],
        examplesTitle: t(`${codexConfigHelpKey}.examplesTitle`),
        examples: codexConfigExamples,
        close: t(`${codexConfigHelpKey}.close`),
        emptyText: ''
      };
    }

    const config = providerOtherConfigHelp[channelType] || {
      examples: [
        {
          title: t(`${otherConfigHelpKey}.examples.opaque`),
          value: `{
  "vendor_extra": {
    "owner": "ops"
  }
}`
        }
      ]
    };
    const providerFields = config.providerFields || [];
    const commonFields = (config.commonFieldKeys || []).map((field) => commonOtherConfigFields[field]).filter(Boolean);
    const opaqueFields = commonOpaqueFieldKeys.map((field) => commonOtherConfigFields[field]);
    const channelName = CHANNEL_OPTIONS[channelType]?.text || customizeT(inputLabel.other);
    return {
      title: t(`${otherConfigHelpKey}.title`, { channel: channelName }),
      intro: t(`${otherConfigHelpKey}.intro`),
      sections: [
        {
          title: t(`${otherConfigHelpKey}.providerFieldsTitle`),
          fields: providerFields,
          emptyText: t(`${otherConfigHelpKey}.noProviderFields`)
        },
        { title: t(`${otherConfigHelpKey}.commonFieldsTitle`), fields: commonFields },
        { title: t(`${otherConfigHelpKey}.opaqueFieldsTitle`), fields: opaqueFields }
      ],
      examplesTitle: t(`${otherConfigHelpKey}.examplesTitle`),
      examples: config.examples || [],
      close: t(`${otherConfigHelpKey}.close`)
    };
  };
  const renderOtherConfigFields = (fields, emptyText) =>
    fields.length > 0 ? (
      <Box sx={{ display: 'grid', gap: 1, mb: 2 }}>
        {fields.map(([field, defaultValue, accepted, description]) => (
          <Box
            key={field}
            sx={{
              display: 'grid',
              gridTemplateColumns: { xs: '1fr', sm: '210px 120px 1fr' },
              gap: 1,
              p: 1,
              borderRadius: 1,
              backgroundColor: theme.palette.mode === 'dark' ? 'grey.900' : 'grey.50'
            }}
          >
            <Typography component="code" variant="caption" sx={{ fontWeight: 700 }}>
              {field}
            </Typography>
            <Typography variant="caption">{t(`${codexConfigHelpKey}.defaultValue`, { value: defaultValue })}</Typography>
            <Typography variant="caption">{t(`${codexConfigHelpKey}.acceptedValue`, { accepted, description })}</Typography>
          </Box>
        ))}
      </Box>
    ) : (
      <Typography variant="caption" color="text.secondary" sx={{ display: 'block', mb: 2 }}>
        {emptyText}
      </Typography>
    );

  const initChannel = (typeValue) => {
    if (typeConfig[typeValue]?.inputLabel) {
      setInputLabel({ ...defaultConfig.inputLabel, ...typeConfig[typeValue].inputLabel });
    } else {
      setInputLabel(defaultConfig.inputLabel);
    }

    if (typeConfig[typeValue]?.prompt) {
      setInputPrompt({ ...defaultConfig.prompt, ...typeConfig[typeValue].prompt });
    } else {
      setInputPrompt(defaultConfig.prompt);
    }

    return typeConfig[typeValue]?.input;
  };

  const handleTypeChange = (setFieldValue, typeValue, values) => {
    if (typeValue === 101) {
      setBatchAdd(false);
    }

    if (Number(typeValue) === 8 && endpointDefinitions) {
      setFieldValue('plugin', values.plugin?.endpoints ? values.plugin : { endpoints: createEndpointPreset(endpointDefinitions) });
    }

    // 处理插件事务
    if (pluginList[typeValue]) {
      const newPluginValues = {};
      const pluginConfig = pluginList[typeValue];
      for (const pluginName in pluginConfig) {
        const plugin = pluginConfig[pluginName];
        const oldValve = values['plugin'] ? values['plugin'][pluginName] || {} : {};
        newPluginValues[pluginName] = {};
        for (const paramName in plugin.params) {
          const param = plugin.params[paramName];
          newPluginValues[pluginName][paramName] = oldValve[paramName] || (param.type === 'bool' ? false : '');
        }
      }
      setFieldValue('plugin', newPluginValues);
    }

    const newInput = initChannel(typeValue);

    if (newInput) {
      Object.keys(newInput).forEach((key) => {
        if (
          (!Array.isArray(values[key]) && values[key] !== null && values[key] !== undefined && values[key] !== '') ||
          (Array.isArray(values[key]) && values[key].length > 0)
        ) {
          return;
        }

        if (key === 'models') {
          setFieldValue(key, initialModel(newInput[key]));
          return;
        }
        setFieldValue(key, newInput[key]);
      });
    }
  };

  const modelOptionsById = useMemo(() => new Map(modelOptions.map((option) => [option.id, option])), [modelOptions]);

  const basicModels = (channelType) => {
    let modelGroup = typeConfig[channelType]?.modelGroup || defaultConfig.modelGroup;
    // 循环 modelOptions，找到 modelGroup 对应的模型
    let modelList = [];
    modelOptions.forEach((model) => {
      if (model.group === modelGroup) {
        modelList.push(model);
      }
    });
    return modelList;
  };

  const handleModelSelectorConfirm = (selectedModels, overwriteModels) => {
    if (tempSetFieldValue && tempFormikValues) {
      if (overwriteModels) {
        // 覆盖模式：清空现有的模型列表，使用选择器中的模型
        tempSetFieldValue('models', selectedModels);
      } else {
        // 追加模式：合并现有模型和新选择的模型，避免重复
        const existingModels = tempFormikValues.models || [];
        const existingModelIds = new Set(existingModels.map((model) => model.id));

        // 过滤掉已存在的模型，避免重复
        const newModels = selectedModels.filter((model) => !existingModelIds.has(model.id));

        // 合并模型列表
        tempSetFieldValue('models', [...existingModels, ...newModels]);
      }
    }
  };

  const mergeBatchContent = (currentValue, importedValue) => {
    if (!importedValue) {
      return currentValue || '';
    }

    if (!currentValue) {
      return importedValue;
    }

    const separator = currentValue.endsWith('\n') || importedValue.startsWith('\n') ? '' : '\n';
    return `${currentValue}${separator}${importedValue}`;
  };

  const normalizeBatchFileContent = (content) =>
    content
      .replace(/^\uFEFF/, '')
      .replace(/\r\n?/g, '\n')
      .split('\n')
      .map((line) => line.trim())
      .filter(Boolean)
      .join('\n');

  const handleBatchFileImport = async (event, currentValue, setFieldValue) => {
    const input = event.target;
    const files = Array.from(input.files || []);

    if (files.length === 0) {
      return;
    }

    setBatchFileImporting(true);

    try {
      const contents = await Promise.all(
        files.map(async (file) => {
          const text = await file.text();
          return normalizeBatchFileContent(text);
        })
      );

      const mergedContent = contents.filter(Boolean).join('\n');

      if (!mergedContent.trim()) {
        showError(t('channel_edit.batchUploadEmpty'));
        return;
      }

      setFieldValue('key', mergeBatchContent(currentValue, mergedContent));
      showSuccess(t('channel_edit.batchUploadSuccess', { count: files.length }));
    } catch (error) {
      showError(t('channel_edit.batchUploadError', { message: error.message || error }));
    } finally {
      input.value = '';
      setBatchFileImporting(false);
    }
  };

  const prepareChannelPayload = (sourceValues) => {
    let values = trims(JSON.parse(JSON.stringify(sourceValues)));
    if (Number(sourceValues.type) === 8) values.plugin = structuredClone(sourceValues.plugin);
    const modelMappingModel = [];

    if (!Array.isArray(values.models) || values.models.length === 0) {
      throw new Error(t('channel_edit.requiredModels'));
    }

    if (values.base_url && values.base_url.endsWith('/')) {
      values.base_url = values.base_url.slice(0, values.base_url.length - 1);
    }
    values = normalizeChannelOtherForRequest(values);
    if (values.type === 18 && values.other === '') {
      values.other = '{"api_version":"v3.1"}';
    }

    if (values.model_mapping) {
      try {
        const modelMapping = values.model_mapping.reduce((acc, item) => {
          if (item.key && item.value) {
            acc[item.key] = item.value;
          }
          return acc;
        }, {});
        const cleanedMapping = {};

        for (const [key, value] of Object.entries(modelMapping)) {
          if (key && value && !(key in cleanedMapping)) {
            cleanedMapping[key] = value;
            modelMappingModel.push(key);
          }
        }

        values.model_mapping = JSON.stringify(cleanedMapping, null, 2);
      } catch (error) {
        throw new Error('Error parsing model_mapping: ' + error.message);
      }
    }

    if (values.model_headers) {
      try {
        const modelHeader = values.model_headers.reduce((acc, item) => {
          if (item.key && item.value) {
            acc[item.key] = item.value;
          }
          return acc;
        }, {});
        const cleanedHeader = {};

        for (const [key, value] of Object.entries(modelHeader)) {
          if (key && value && !(key in cleanedHeader)) {
            cleanedHeader[key] = value;
          }
        }

        values.model_headers = JSON.stringify(cleanedHeader, null, 2);
      } catch (error) {
        throw new Error('Error parsing model_headers: ' + error.message);
      }
    }

    if (values.custom_parameter) {
      values.custom_parameter = normalizeJSONObjectString('custom_parameter', values.custom_parameter);
    }

    if (values.other) {
      values.other = normalizeJSONObjectString('other', values.other);
    }

    if (values.disabled_stream) {
      values.disabled_stream = removeDuplicates(values.disabled_stream);
    }

    const existingModelIds = values.models.map((model) => model.id);
    const newModelIds = modelMappingModel.filter((id) => !existingModelIds.includes(id));
    const allUniqueModelIds = Array.from(new Set([...existingModelIds, ...newModelIds]));

    values.models = allUniqueModelIds.join(',');
    values.group = values.groups.join(',');

    return values;
  };

  const handleCodexBatchAuthFilesImport = async (event, values) => {
    const input = event.target;
    const files = Array.from(input.files || []);

    if (files.length === 0) {
      return;
    }

    setCodexBatchAuthFileImporting(true);

    try {
      const payload = prepareChannelPayload(values);
      const formData = new FormData();
      formData.append('channel', JSON.stringify(payload));
      files.forEach((file) => {
        formData.append('files', file);
      });

      const res = await API.post('/api/codex/auth-files/import', formData, {
        headers: {
          'Content-Type': 'multipart/form-data'
        }
      });

      if (!res.data.success) {
        showError(res.data.message || 'Failed to batch import auth files');
        return;
      }

      showSuccess(`Imported ${res.data.data.count} Codex auth files`);
      onOk(true);
    } catch (error) {
      showError(error.message || error);
    } finally {
      input.value = '';
      setCodexBatchAuthFileImporting(false);
    }
  };

  const submit = async (values, { setErrors, setStatus, setSubmitting }) => {
    setSubmitting(true);
    const session = editSession.current;
    const baseApiUrl = isTag ? '/api/channel_tag/' + encodeURIComponent(channelId) : '/api/channel/';
    try {
      const payload = prepareChannelPayload(values);
      if (channelId && (isTag || !payload.key?.trim() || payload.key === initialInput.key)) {
        delete payload.key;
      }
      let res;
      if (channelId) {
        const update = { ...payload, id: parseInt(channelId) };
        const confirmed = await new Promise((resolve) => {
          editConfirmationResolver.current = resolve;
          setEditConfirmationOpen(true);
        });
        if (!confirmed || session !== editSession.current) return;
        res = await API.put(baseApiUrl, update);
      } else {
        res = await API.post(baseApiUrl, payload);
      }
      if (session !== editSession.current) return;
      const { success, message } = res.data;
      if (!success) {
        throw new Error(message);
      }
      showSuccess(t(channelId ? 'channel_edit.editSuccess' : 'channel_edit.addSuccess'));
      setStatus({ success: true });
      onOk(true);
    } catch (error) {
      setStatus({ success: false });
      showError(error.message);
      setErrors({ submit: error.message });
    } finally {
      setSubmitting(false);
    }
  };

  function initialModel(channelModel) {
    if (!channelModel) {
      return [];
    }

    // 如果 channelModel 是一个字符串
    if (typeof channelModel === 'string') {
      channelModel = channelModel.split(',');
    }
    let modelList = channelModel.map((model) => {
      const modelOption = modelOptionsById.get(model);
      if (modelOption) {
        return modelOption;
      }
      return { id: model, group: t('channel_edit.customModelTip') };
    });
    return modelList;
  }

  const loadChannel = async () => {
    try {
      let baseApiUrl = `/api/channel/${channelId}`;

      if (isTag) {
        baseApiUrl = '/api/channel_tag/' + encodeURIComponent(channelId);
      }

      let res = await API.get(baseApiUrl);
      const { success, message, data } = res.data;
      if (success) {
        if (data.models === '') {
          data.models = [];
        } else {
          data.models = initialModel(data.models);
        }
        if (data.group === '') {
          data.groups = [];
        } else {
          data.groups = data.group.split(',');
        }

        data.model_mapping =
          data.model_mapping !== ''
            ? Object.entries(JSON.parse(data.model_mapping)).map(([key, value], index) => ({
                index,
                key,
                value
              }))
            : [];
        // if (data.model_headers) {
        data.model_headers =
          data.model_headers !== ''
            ? Object.entries(JSON.parse(data.model_headers)).map(([key, value], index) => ({
                index,
                key,
                value
              }))
            : [];
        // }

        // Format the custom_parameter JSON for better readability if it's not empty
        if (data.custom_parameter !== '') {
          try {
            // Parse and then stringify with indentation for formatting
            const parsedJson = JSON.parse(data.custom_parameter);
            data.custom_parameter = JSON.stringify(parsedJson, null, 2);
          } catch (error) {
            // If parsing fails, keep the original string
            console.log('Error parsing custom_parameter JSON:', error);
          }
        } else {
          data.custom_parameter = '';
        }

        if (data.type === 101 && data.other !== '') {
          try {
            data.other = JSON.stringify(JSON.parse(data.other), null, 2);
          } catch (error) {
            console.log('Error parsing other JSON:', error);
          }
        }

        data.base_url = data.base_url ?? '';
        data.is_edit = true;
        if (data.plugin === null) {
          data.plugin = {};
        }
        initChannel(data.type);
        setInitialInput(data);

        if (!isTag && data.tag) {
          setHasTag(true);
        }
      } else {
        showError(message);
      }
    } catch (error) {
      return;
    }
  };

  const normalizeJSONObjectString = (fieldName, value) => {
    const trimmed = value?.trim();
    if (!trimmed) {
      return '';
    }

    let parsedJson;
    try {
      parsedJson = JSON.parse(trimmed);
    } catch (error) {
      throw new Error(`Error parsing ${fieldName}: ${error.message}`);
    }

    if (parsedJson === null || Array.isArray(parsedJson) || typeof parsedJson !== 'object') {
      throw new Error(`${fieldName} must be a JSON object`);
    }

    return JSON.stringify(parsedJson, null, 2);
  };

  useEffect(() => {
    if (open) {
      setBatchAdd(isTag);
      if (channelId) {
        loadChannel().then();
      } else {
        setHasTag(false);
        initChannel(1);
        setInitialInput({ ...defaultConfig.input, is_edit: false });
      }
    }

    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [channelId, open]);

  return (
    <Dialog open={open} onClose={onCancel} fullWidth maxWidth={'md'}>
      <ConfirmDialog
        open={editConfirmationOpen}
        title="确认保存渠道修改"
        aria-label="确认保存渠道修改"
        aria-describedby="channel-edit-impact-description"
        content={
          <Box id="channel-edit-impact-description">
            <Typography sx={{ mb: 1 }}>
              {isTag ? '本次修改将应用到该标签下的渠道。' : '本次修改将保存到当前渠道，渠道 ID 保持不变。'}
            </Typography>
            <Typography sx={{ mb: 1 }}>
              修改地址、账号或凭据可能导致已有任务的查询、续作、结果获取，以及已存储响应、文件和会话等上游资源的后续访问失败。
            </Typography>
            <Typography variant="body2">模型、分组、路由或功能配置的修改也会影响后续请求。确认后保存，取消则保留当前表单。</Typography>
          </Box>
        }
        onClose={() => resolveEditConfirmation(false)}
        action={
          <Button color="warning" variant="contained" onClick={() => resolveEditConfirmation(true)}>
            确认保存
          </Button>
        }
      />
      <DialogTitle sx={{ margin: '0px', fontWeight: 700, lineHeight: '1.55556', padding: '24px', fontSize: '1.125rem' }}>
        {channelId ? t('common.edit') : t('common.create')}
      </DialogTitle>
      <Divider />
      <DialogContent>
        <ChannelForm initialValues={initialInput} enableReinitialize validationSchema={validationSchema} onSubmit={submit}>
          {({ errors, handleBlur, handleChange, handleSubmit, isSubmitting, touched, values, setFieldValue, getValues }) => {
            // 保存当前Formik状态，以便在模型选择器中使用
            const openModelSelector = () => {
              setTempFormikValues({ ...getValues() });
              setTempSetFieldValue(() => setFieldValue); // 保存函数引用
              setModelSelectorOpen(true);
            };
            const activeOtherConfigHelp = buildOtherConfigHelp(values.type);
            const showResponsesWSSelfHostedWarning = isSelfHostedResponsesWSEnabled(values.other);
            const otherHelperTextId = 'helper-text-channel-other-label';
            const otherWarningId = 'helper-text-channel-other-self-hosted-warning';
            const otherDescriptionIds = showResponsesWSSelfHostedWarning ? `${otherHelperTextId} ${otherWarningId}` : otherHelperTextId;

            return (
              <form noValidate onSubmit={handleSubmit}>
                {channelId && (
                  <Typography role="note" color="text.secondary" sx={{ mb: 2 }}>
                    保存前会提示修改的影响范围，确认后原地更新渠道。凭据留空则保留原值。
                  </Typography>
                )}
                {!isTag && (
                  <FormControl fullWidth error={Boolean(touched.type && errors.type)} sx={{ ...theme.typography.otherInput }}>
                    <InputLabel htmlFor="channel-type-label">{customizeT(inputLabel.type)}</InputLabel>
                    <Select
                      id="channel-type-label"
                      label={customizeT(inputLabel.type)}
                      value={values.type}
                      name="type"
                      onBlur={handleBlur}
                      onChange={(e) => {
                        handleChange(e);
                        handleTypeChange(setFieldValue, e.target.value, getValues());
                      }}
                      disabled={hasTag}
                      MenuProps={{
                        PaperProps: {
                          style: {
                            maxHeight: 200
                          }
                        }
                      }}
                    >
                      {Object.values(CHANNEL_OPTIONS).map((option) => {
                        return (
                          <MenuItem
                            key={option.value}
                            value={option.value}
                            disabled={Number(option.value) === 8 && (!endpointDefinitions || endpointLoadError)}
                          >
                            {option.text}
                          </MenuItem>
                        );
                      })}
                    </Select>
                    {touched.type && errors.type ? (
                      <FormHelperText error id="helper-tex-channel-type-label">
                        {errors.type}
                      </FormHelperText>
                    ) : (
                      <FormHelperText id="helper-tex-channel-type-label"> {customizeT(inputPrompt.type)} </FormHelperText>
                    )}
                    {endpointLoadError && Number(values.type) !== 8 && (
                      <FormHelperText error role="alert">
                        {t('channel_edit.endpoints.loadError')}
                      </FormHelperText>
                    )}
                  </FormControl>
                )}

                <FormControl fullWidth error={Boolean(touched.tag && errors.tag)} sx={{ ...theme.typography.otherInput }}>
                  <InputLabel htmlFor="channel-tag-label">{customizeT(inputLabel.tag)}</InputLabel>
                  <OutlinedInput
                    id="channel-tag-label"
                    label={customizeT(inputLabel.tag)}
                    type="text"
                    value={values.tag}
                    name="tag"
                    onBlur={handleBlur}
                    onChange={handleChange}
                    inputProps={{}}
                    aria-describedby="helper-text-channel-tag-label"
                  />
                  {touched.tag && errors.tag ? (
                    <FormHelperText error id="helper-tex-channel-tag-label">
                      {errors.tag}
                    </FormHelperText>
                  ) : (
                    <FormHelperText id="helper-tex-channel-tag-label"> {customizeT(inputPrompt.tag)} </FormHelperText>
                  )}
                </FormControl>

                {!isTag && (
                  <FormControl fullWidth error={Boolean(touched.name && errors.name)} sx={{ ...theme.typography.otherInput }}>
                    <InputLabel htmlFor="channel-name-label">{customizeT(inputLabel.name)}</InputLabel>
                    <OutlinedInput
                      id="channel-name-label"
                      label={customizeT(inputLabel.name)}
                      type="text"
                      value={values.name}
                      name="name"
                      onBlur={handleBlur}
                      onChange={handleChange}
                      inputProps={{ autoComplete: 'name' }}
                      aria-describedby="helper-text-channel-name-label"
                    />
                    {touched.name && errors.name ? (
                      <FormHelperText error id="helper-tex-channel-name-label">
                        {errors.name}
                      </FormHelperText>
                    ) : (
                      <FormHelperText id="helper-tex-channel-name-label"> {customizeT(inputPrompt.name)} </FormHelperText>
                    )}
                  </FormControl>
                )}
                {inputPrompt.base_url && (
                  <FormControl fullWidth error={Boolean(touched.base_url && errors.base_url)} sx={{ ...theme.typography.otherInput }}>
                    <InputLabel htmlFor="channel-base_url-label">{customizeT(inputLabel.base_url)}</InputLabel>
                    <OutlinedInput
                      id="channel-base_url-label"
                      label={customizeT(inputLabel.base_url)}
                      type="text"
                      value={values.base_url}
                      name="base_url"
                      onBlur={handleBlur}
                      onChange={handleChange}
                      inputProps={{}}
                      aria-describedby="helper-text-channel-base_url-label"
                    />

                    {touched.base_url && errors.base_url ? (
                      <FormHelperText error id="helper-tex-channel-base_url-label">
                        {errors.base_url}
                      </FormHelperText>
                    ) : (
                      <FormHelperText id="helper-tex-channel-base_url-label"> {customizeT(inputPrompt.base_url)} </FormHelperText>
                    )}
                  </FormControl>
                )}

                {typeConfig[Number(values.type)]?.fields?.other === true && (
                  <Box sx={{ ...theme.typography.otherInput }}>
                    <FormControl fullWidth error={Boolean(touched.other && errors.other)}>
                      <InputLabel htmlFor="channel-other-label">{customizeT(inputLabel.other)}</InputLabel>
                      <OutlinedInput
                        id="channel-other-label"
                        label={customizeT(inputLabel.other)}
                        type="text"
                        multiline={values.type === 101}
                        minRows={values.type === 101 ? 4 : undefined}
                        value={values.other}
                        name="other"
                        disabled={hasTag}
                        onBlur={handleBlur}
                        onChange={handleChange}
                        endAdornment={
                          activeOtherConfigHelp ? (
                            <InputAdornment position="end" sx={{ alignSelf: values.type === 101 ? 'flex-start' : 'center', mt: 0.5 }}>
                              <Tooltip
                                title={values.type === 101 ? t(`${codexConfigHelpKey}.tooltip`) : t(`${otherConfigHelpKey}.tooltip`)}
                              >
                                <IconButton
                                  aria-label={values.type === 101 ? t(`${codexConfigHelpKey}.tooltip`) : t(`${otherConfigHelpKey}.tooltip`)}
                                  edge="end"
                                  size="small"
                                  type="button"
                                  onClick={() => setOtherConfigHelpOpen(true)}
                                >
                                  <Icon icon="solar:question-circle-bold-duotone" width={20} />
                                </IconButton>
                              </Tooltip>
                            </InputAdornment>
                          ) : null
                        }
                        inputProps={{ 'aria-describedby': otherDescriptionIds }}
                      />
                      {touched.other && errors.other ? (
                        <FormHelperText error id={otherHelperTextId}>
                          {errors.other}
                        </FormHelperText>
                      ) : (
                        <FormHelperText id={otherHelperTextId}> {customizeT(inputPrompt.other)} </FormHelperText>
                      )}
                      {showResponsesWSSelfHostedWarning && (
                        <FormHelperText id={otherWarningId} role="alert" sx={{ color: 'warning.main', fontWeight: 600 }}>
                          {t('channel_edit.responsesWSSelfHostedWarning')}
                        </FormHelperText>
                      )}
                    </FormControl>

                    {activeOtherConfigHelp && (
                      <Dialog open={otherConfigHelpOpen} onClose={() => setOtherConfigHelpOpen(false)} fullWidth maxWidth="md">
                        <DialogTitle>{activeOtherConfigHelp.title}</DialogTitle>
                        <DialogContent dividers>
                          <Typography variant="body2" sx={{ mb: 2 }}>
                            {activeOtherConfigHelp.intro}
                          </Typography>
                          {activeOtherConfigHelp.sections.map((section) => (
                            <Box key={section.title}>
                              <Typography variant="subtitle2" sx={{ mb: 1 }}>
                                {section.title}
                              </Typography>
                              {renderOtherConfigFields(section.fields || [], section.emptyText || activeOtherConfigHelp.emptyText)}
                            </Box>
                          ))}
                          <Typography variant="subtitle2" sx={{ mb: 1 }}>
                            {activeOtherConfigHelp.examplesTitle}
                          </Typography>
                          <Box sx={{ display: 'grid', gridTemplateColumns: { xs: '1fr', md: '1fr 1fr' }, gap: 1.5 }}>
                            {activeOtherConfigHelp.examples.map((example) => (
                              <Box key={example.title}>
                                <Typography variant="caption" sx={{ display: 'block', mb: 0.5, fontWeight: 700 }}>
                                  {example.title}
                                </Typography>
                                <Box
                                  component="pre"
                                  sx={{
                                    m: 0,
                                    p: 1.25,
                                    borderRadius: 1,
                                    overflowX: 'auto',
                                    backgroundColor: theme.palette.mode === 'dark' ? 'grey.900' : 'grey.100',
                                    fontSize: '0.75rem',
                                    lineHeight: 1.5
                                  }}
                                >
                                  {example.value}
                                </Box>
                              </Box>
                            ))}
                          </Box>
                        </DialogContent>
                        <DialogActions>
                          <Button onClick={() => setOtherConfigHelpOpen(false)}>{activeOtherConfigHelp.close}</Button>
                        </DialogActions>
                      </Dialog>
                    )}
                  </Box>
                )}

                <FormControl fullWidth sx={{ ...theme.typography.otherInput }}>
                  <Autocomplete
                    multiple
                    id="channel-groups-label"
                    options={groupOptions}
                    value={values.groups}
                    disabled={hasTag}
                    onChange={(e, value) => {
                      const event = {
                        target: {
                          name: 'groups',
                          value: value
                        }
                      };
                      handleChange(event);
                    }}
                    onBlur={handleBlur}
                    filterSelectedOptions
                    renderInput={(params) => (
                      <TextField {...params} name="groups" error={Boolean(errors.groups)} label={customizeT(inputLabel.groups)} />
                    )}
                    aria-describedby="helper-text-channel-groups-label"
                  />
                  {errors.groups ? (
                    <FormHelperText error id="helper-tex-channel-groups-label">
                      {errors.groups}
                    </FormHelperText>
                  ) : (
                    <FormHelperText id="helper-tex-channel-groups-label"> {customizeT(inputPrompt.groups)} </FormHelperText>
                  )}
                </FormControl>

                <ChannelModelsField
                  options={modelOptions}
                  disabled={hasTag}
                  label={customizeT(inputLabel.models)}
                  helperText={customizeT(inputPrompt.models)}
                  customGroupLabel={t('channel_edit.customModelTip')}
                />
                <Container
                  sx={{
                    textAlign: 'right'
                  }}
                >
                  <ButtonGroup variant="outlined" aria-label="small outlined primary button group">
                    <Button
                      size="small"
                      onClick={() => {
                        const modelString = getValues()
                          .models.map((model) => model.id)
                          .join(',');
                        copy(modelString);
                      }}
                    >
                      {isMobile ? <Icon icon="mdi:content-copy" /> : t('channel_edit.copyModels')}
                    </Button>
                    <Button
                      disabled={hasTag}
                      size="small"
                      onClick={() => {
                        setFieldValue('models', basicModels(values.type));
                      }}
                    >
                      {isMobile ? <Icon icon="mdi:playlist-plus" /> : t('channel_edit.inputChannelModel')}
                    </Button>
                    {/* <Button
                      disabled={hasTag}
                      size="small"
                      onClick={() => {
                        setFieldValue('models', modelOptions);
                      }}
                    >
                      {t('channel_edit.inputAllModel')}
                    </Button> */}
                    {inputLabel.provider_models_list && (
                      <Tooltip title={customizeT(inputPrompt.provider_models_list)} placement="top">
                        <Button
                          disabled={hasTag}
                          size="small"
                          onClick={openModelSelector}
                          startIcon={!isMobile && <Icon icon="mdi:cloud-download" />}
                        >
                          {isMobile ? <Icon icon="mdi:cloud-download" /> : customizeT(inputLabel.provider_models_list)}
                        </Button>
                      </Tooltip>
                    )}
                  </ButtonGroup>
                </Container>
                <FormControl fullWidth error={Boolean(touched.key && errors.key)} sx={{ ...theme.typography.otherInput }}>
                  {!batchAdd ? (
                    <>
                      {values.type === 101 ? (
                        <TextField
                          multiline
                          fullWidth
                          id="channel-key-label"
                          label={customizeT(inputLabel.key)}
                          value={values.key}
                          name="key"
                          disabled={isTag}
                          onBlur={handleBlur}
                          onChange={handleChange}
                          aria-describedby="helper-text-channel-key-label"
                          minRows={4}
                          maxRows={12}
                        />
                      ) : (
                        <>
                          <InputLabel htmlFor="channel-key-label">{customizeT(inputLabel.key)}</InputLabel>
                          <OutlinedInput
                            id="channel-key-label"
                            label={customizeT(inputLabel.key)}
                            type="text"
                            value={values.key}
                            name="key"
                            disabled={isTag}
                            onBlur={handleBlur}
                            onChange={handleChange}
                            inputProps={{}}
                            aria-describedby="helper-text-channel-key-label"
                          />
                        </>
                      )}
                    </>
                  ) : (
                    <Box>
                      <TextField
                        multiline
                        fullWidth
                        id="channel-key-label"
                        label={customizeT(inputLabel.key)}
                        value={values.key}
                        name="key"
                        disabled={isTag}
                        onBlur={handleBlur}
                        onChange={handleChange}
                        aria-describedby="helper-text-channel-key-label"
                        minRows={5}
                        maxRows={15}
                        placeholder={customizeT(inputPrompt.key) + t('channel_edit.batchKeytip')}
                      />
                      {channelId === 0 && (
                        <>
                          <input
                            ref={batchFileInputRef}
                            hidden
                            multiple
                            type="file"
                            onChange={(event) => handleBatchFileImport(event, values.key, setFieldValue)}
                          />
                          {values.type !== 101 && (
                            <Box
                              sx={{
                                mt: 1,
                                display: 'flex',
                                gap: 1,
                                alignItems: 'center',
                                justifyContent: 'space-between',
                                flexWrap: 'wrap'
                              }}
                            >
                              <Button
                                size="small"
                                variant="outlined"
                                disabled={batchFileImporting}
                                startIcon={
                                  batchFileImporting ? <Icon icon="svg-spinners:3-dots-scale" /> : <Icon icon="solar:upload-bold-duotone" />
                                }
                                onClick={() => batchFileInputRef.current?.click()}
                              >
                                {t('channel_edit.batchUploadFiles')}
                              </Button>
                              <Typography variant="caption" color="text.secondary">
                                {t('channel_edit.batchUploadFilesTip')}
                              </Typography>
                            </Box>
                          )}
                        </>
                      )}
                    </Box>
                  )}

                  {touched.key && errors.key ? (
                    <FormHelperText error id="helper-tex-channel-key-label">
                      {errors.key}
                    </FormHelperText>
                  ) : (
                    <FormHelperText id="helper-tex-channel-key-label">
                      <Box sx={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
                        <span>{customizeT(inputPrompt.key)}</span>
                        {channelId === 0 && values.type !== 101 && (
                          <Box sx={{ display: 'flex', alignItems: 'center', gap: 1 }}>
                            <Switch size="small" checked={Boolean(batchAdd)} onChange={(e) => setBatchAdd(e.target.checked)} />
                            <Typography variant="body2">{t('channel_edit.batchAdd')}</Typography>
                          </Box>
                        )}
                      </Box>
                    </FormHelperText>
                  )}
                </FormControl>

                {values.type === 101 && !batchAdd && !isTag && (
                  <Box sx={{ mt: 2, mb: 2 }}>
                    <CodexAuthControls
                      channelId={isTag ? 0 : channelId}
                      proxy={values.proxy}
                      currentName={values.name}
                      onCredentials={(credentials) => setFieldValue('key', credentials)}
                      onSuggestedName={(suggestedName) => {
                        if (!values.name && suggestedName) {
                          setFieldValue('name', suggestedName);
                        }
                      }}
                      authFileActions={
                        channelId === 0 ? (
                          <>
                            <input
                              ref={codexBatchAuthFileInputRef}
                              hidden
                              multiple
                              type="file"
                              accept=".json,application/json"
                              onChange={(event) => handleCodexBatchAuthFilesImport(event, getValues())}
                            />
                            <Button
                              variant="outlined"
                              color="secondary"
                              disabled={codexBatchAuthFileImporting}
                              onClick={() => codexBatchAuthFileInputRef.current?.click()}
                              startIcon={codexBatchAuthFileImporting ? null : <Icon icon="solar:folder-with-files-bold-duotone" />}
                            >
                              {codexBatchAuthFileImporting ? 'Importing auth files...' : 'Batch Import Auth Files'}
                            </Button>
                          </>
                        ) : null
                      }
                    />
                  </Box>
                )}

                {inputPrompt.model_mapping && (
                  <FormControl
                    fullWidth
                    error={Boolean(touched.model_mapping && errors.model_mapping)}
                    sx={{ ...theme.typography.otherInput }}
                  >
                    <MapInput
                      mapValue={values.model_mapping}
                      onChange={(newValue) => {
                        setFieldValue('model_mapping', newValue);
                      }}
                      disabled={hasTag}
                      error={Boolean(touched.model_mapping && errors.model_mapping)}
                      label={{
                        keyName: customizeT(inputLabel.model_mapping),
                        valueName: customizeT(inputPrompt.model_mapping),
                        name: customizeT(inputLabel.model_mapping)
                      }}
                    />
                    {touched.model_mapping && errors.model_mapping ? (
                      <FormHelperText error id="helper-tex-channel-model_mapping-label">
                        {errors.model_mapping}
                      </FormHelperText>
                    ) : (
                      <FormHelperText id="helper-tex-channel-model_mapping-label">{customizeT(inputPrompt.model_mapping)}</FormHelperText>
                    )}
                  </FormControl>
                )}
                {inputPrompt.model_headers && (
                  <FormControl
                    fullWidth
                    error={Boolean(touched.model_headers && errors.model_headers)}
                    sx={{ ...theme.typography.otherInput }}
                  >
                    <MapInput
                      mapValue={values.model_headers}
                      onChange={(newValue) => {
                        setFieldValue('model_headers', newValue);
                      }}
                      disabled={hasTag}
                      error={Boolean(touched.model_headers && errors.model_headers)}
                      label={{
                        keyName: customizeT(inputLabel.model_headers),
                        valueName: customizeT(inputPrompt.model_headers),
                        name: customizeT(inputLabel.model_headers)
                      }}
                    />
                    {touched.model_headers && errors.model_headers ? (
                      <FormHelperText error id="helper-tex-channel-model_headers-label">
                        {errors.model_headers}
                      </FormHelperText>
                    ) : (
                      <FormHelperText id="helper-tex-channel-model_headers-label">{customizeT(inputPrompt.model_headers)}</FormHelperText>
                    )}
                  </FormControl>
                )}
                {inputPrompt.custom_parameter && (
                  <FormControl
                    fullWidth
                    error={Boolean(touched.custom_parameter && errors.custom_parameter)}
                    sx={{ ...theme.typography.otherInput }}
                  >
                    <InputLabel shrink htmlFor="channel-custom_parameter-label">
                      {customizeT(inputLabel.custom_parameter)}
                    </InputLabel>
                    <Box
                      sx={{
                        border: '1px solid',
                        borderColor: touched.custom_parameter && errors.custom_parameter ? 'error.main' : 'divider',
                        borderRadius: 1,
                        overflow: 'hidden',
                        marginTop: 2, // Add some margin for the label
                        resize: 'vertical',
                        height: '200px',
                        minHeight: '100px',
                        '&:hover': {
                          borderColor: 'primary.main'
                        },
                        '&:focus-within': {
                          borderColor: 'primary.main',
                          borderWidth: 2
                        }
                      }}
                    >
                      <CustomParameterEditor />
                    </Box>
                    {touched.custom_parameter && errors.custom_parameter ? (
                      <FormHelperText error id="helper-tex-channel-custom_parameter-label">
                        {errors.custom_parameter}
                      </FormHelperText>
                    ) : (
                      <FormHelperText id="helper-tex-channel-custom_parameter-label">
                        {customizeT(inputPrompt.custom_parameter)}
                      </FormHelperText>
                    )}
                  </FormControl>
                )}
                {inputPrompt.disabled_stream && (
                  <FormControl
                    fullWidth
                    error={Boolean(touched.disabled_stream && errors.disabled_stream)}
                    sx={{ ...theme.typography.otherInput }}
                  >
                    <ListInput
                      listValue={values.disabled_stream}
                      onChange={(newValue) => {
                        setFieldValue('disabled_stream', newValue);
                      }}
                      disabled={hasTag}
                      error={Boolean(touched.disabled_stream && errors.disabled_stream)}
                      label={{
                        name: customizeT(inputLabel.disabled_stream),
                        itemName: customizeT(inputPrompt.disabled_stream)
                      }}
                    />
                  </FormControl>
                )}

                <FormControl fullWidth error={Boolean(touched.proxy && errors.proxy)} sx={{ ...theme.typography.otherInput }}>
                  <InputLabel htmlFor="channel-proxy-label">{customizeT(inputLabel.proxy)}</InputLabel>
                  <OutlinedInput
                    id="channel-proxy-label"
                    label={customizeT(inputLabel.proxy)}
                    disabled={hasTag}
                    type="text"
                    value={values.proxy}
                    name="proxy"
                    onBlur={handleBlur}
                    onChange={handleChange}
                    inputProps={{}}
                    aria-describedby="helper-text-channel-proxy-label"
                  />
                  {touched.proxy && errors.proxy ? (
                    <FormHelperText error id="helper-tex-channel-proxy-label">
                      {errors.proxy}
                    </FormHelperText>
                  ) : (
                    <FormHelperText id="helper-tex-channel-proxy-label"> {customizeT(inputPrompt.proxy)} </FormHelperText>
                  )}
                </FormControl>
                {inputPrompt.test_model && (
                  <FormControl fullWidth error={Boolean(touched.test_model && errors.test_model)} sx={{ ...theme.typography.otherInput }}>
                    <InputLabel htmlFor="channel-test_model-label">{customizeT(inputLabel.test_model)}</InputLabel>
                    <OutlinedInput
                      id="channel-test_model-label"
                      label={customizeT(inputLabel.test_model)}
                      type="text"
                      disabled={hasTag}
                      value={values.test_model}
                      name="test_model"
                      onBlur={handleBlur}
                      onChange={handleChange}
                      inputProps={{}}
                      aria-describedby="helper-text-channel-test_model-label"
                    />
                    {touched.test_model && errors.test_model ? (
                      <FormHelperText error id="helper-tex-channel-test_model-label">
                        {errors.test_model}
                      </FormHelperText>
                    ) : (
                      <FormHelperText id="helper-tex-channel-test_model-label"> {customizeT(inputPrompt.test_model)} </FormHelperText>
                    )}
                  </FormControl>
                )}
                {inputPrompt.only_chat && (
                  <FormControl fullWidth>
                    <FormControlLabel
                      control={
                        <Switch
                          disabled={hasTag}
                          checked={Boolean(values.only_chat)}
                          onChange={(event) => {
                            setFieldValue('only_chat', event.target.checked);
                          }}
                        />
                      }
                      label={customizeT(inputLabel.only_chat)}
                    />
                    <FormHelperText id="helper-tex-only_chat_model-label"> {customizeT(inputPrompt.only_chat)} </FormHelperText>
                  </FormControl>
                )}
                {inputPrompt.pre_cost && (
                  <FormControl fullWidth error={Boolean(touched.pre_cost && errors.pre_cost)} sx={{ ...theme.typography.otherInput }}>
                    <InputLabel htmlFor="channel-pre_cost-label">{customizeT(inputLabel.pre_cost)}</InputLabel>
                    <Select
                      id="channel-pre_cost-label"
                      label={customizeT(inputLabel.pre_cost)}
                      value={values.pre_cost}
                      name="pre_cost"
                      onBlur={handleBlur}
                      onChange={handleChange}
                      disabled={hasTag}
                      MenuProps={{
                        PaperProps: {
                          style: {
                            maxHeight: 200
                          }
                        }
                      }}
                    >
                      {PreCostType.map((option) => {
                        return (
                          <MenuItem key={option.value} value={option.value}>
                            {option.label}
                          </MenuItem>
                        );
                      })}
                    </Select>
                    {touched.pre_cost && errors.pre_cost ? (
                      <FormHelperText error id="helper-tex-channel-pre_cost-label">
                        {errors.pre_cost}
                      </FormHelperText>
                    ) : (
                      <FormHelperText id="helper-tex-channel-pre_cost-label"> {customizeT(inputPrompt.pre_cost)} </FormHelperText>
                    )}
                  </FormControl>
                )}
                {inputPrompt.compatible_response && (
                  <FormControl fullWidth>
                    <FormControlLabel
                      control={
                        <Switch
                          disabled={hasTag}
                          checked={Boolean(values.compatible_response)}
                          onChange={(event) => {
                            setFieldValue('compatible_response', event.target.checked);
                          }}
                        />
                      }
                      label={customizeT(inputLabel.compatible_response)}
                    />
                    <FormHelperText id="helper-tex-compatible_response-label">{customizeT(inputPrompt.compatible_response)}</FormHelperText>
                  </FormControl>
                )}
                {inputPrompt.allow_extra_body && (
                  <FormControl fullWidth>
                    <FormControlLabel
                      control={
                        <Switch
                          disabled={hasTag}
                          checked={Boolean(values.allow_extra_body)}
                          onChange={(event) => {
                            setFieldValue('allow_extra_body', event.target.checked);
                          }}
                        />
                      }
                      label={customizeT(inputLabel.allow_extra_body)}
                    />
                    <FormHelperText id="helper-tex-allow_extra_body-label">{customizeT(inputPrompt.allow_extra_body)}</FormHelperText>
                  </FormControl>
                )}
                {Number(values.type) === 8 &&
                  (endpointLoadError || !endpointDefinitions ? (
                    <Typography role={endpointLoadError ? 'alert' : 'status'} color={endpointLoadError ? 'error' : 'text.secondary'}>
                      {t(endpointLoadError ? 'channel_edit.endpoints.loadError' : 'channel_edit.endpoints.loading')}
                    </Typography>
                  ) : (
                    <ChannelEndpointsEditor
                      definitions={endpointDefinitions}
                      plugin={values.plugin}
                      baseURL={values.base_url}
                      disabled={hasTag}
                      onChange={(plugin) => setFieldValue('plugin', plugin)}
                    />
                  ))}
                {pluginList[values.type] &&
                  Object.keys(pluginList[values.type]).map((pluginId) => {
                    const plugin = pluginList[values.type][pluginId];
                    return (
                      <>
                        <Box
                          sx={{
                            border: '1px solid #e0e0e0',
                            borderRadius: 2,
                            marginTop: 2,
                            marginBottom: 2,
                            overflow: 'hidden'
                          }}
                        >
                          <Box
                            sx={{
                              display: 'flex',
                              justifyContent: 'space-between',
                              alignItems: 'center',
                              padding: 2
                            }}
                          >
                            <Box sx={{ flex: 1 }}>
                              <Typography variant="h3">{customizeT(plugin.name)}</Typography>
                              <Typography variant="caption">{customizeT(plugin.description)}</Typography>
                            </Box>
                            <Button
                              onClick={() => setExpanded(!expanded)}
                              endIcon={
                                expanded ? (
                                  <Icon icon="solar:alt-arrow-up-line-duotone" />
                                ) : (
                                  <Icon icon="solar:alt-arrow-down-line-duotone" />
                                )
                              }
                              sx={{ textTransform: 'none', marginLeft: 2 }}
                            >
                              {expanded ? t('channel_edit.collapse') : t('channel_edit.expand')}
                            </Button>
                          </Box>

                          <Collapse in={expanded}>
                            <Box sx={{ padding: 2, marginTop: -3 }}>
                              {Object.keys(plugin.params).map((paramId) => {
                                const param = plugin.params[paramId];
                                const name = `plugin.${pluginId}.${paramId}`;
                                return param.type === 'bool' ? (
                                  <FormControl key={name} fullWidth sx={{ ...theme.typography.otherInput }}>
                                    <FormControlLabel
                                      key={name}
                                      required
                                      control={
                                        <Switch
                                          key={name}
                                          name={name}
                                          disabled={hasTag}
                                          checked={values.plugin?.[pluginId]?.[paramId] || false}
                                          onChange={(event) => {
                                            setFieldValue(name, event.target.checked);
                                          }}
                                        />
                                      }
                                      label={t('channel_edit.isEnable')}
                                    />
                                    <FormHelperText id="helper-tex-channel-key-label"> {customizeT(param.description)} </FormHelperText>
                                  </FormControl>
                                ) : (
                                  <FormControl key={name} fullWidth sx={{ ...theme.typography.otherInput }}>
                                    <TextField
                                      multiline
                                      key={name}
                                      name={name}
                                      disabled={hasTag}
                                      value={values.plugin?.[pluginId]?.[paramId] || ''}
                                      label={customizeT(param.name)}
                                      placeholder={customizeT(param.description)}
                                      onChange={handleChange}
                                    />
                                    <FormHelperText id="helper-tex-channel-key-label"> {customizeT(param.description)} </FormHelperText>
                                  </FormControl>
                                );
                              })}
                            </Box>
                          </Collapse>
                        </Box>
                      </>
                    );
                  })}
                <DialogActions>
                  <Button onClick={onCancel}>{t('common.cancel')}</Button>
                  <Button
                    disableElevation
                    disabled={isSubmitting || (Number(values.type) === 8 && (!endpointDefinitions || endpointLoadError))}
                    type="submit"
                    variant="contained"
                    color="primary"
                  >
                    {t('common.submit')}
                  </Button>
                </DialogActions>
              </form>
            );
          }}
        </ChannelForm>

        {/* 模型选择器弹窗 */}
        <ModelSelectorModal
          open={modelSelectorOpen}
          onClose={() => setModelSelectorOpen(false)}
          onConfirm={(selectedModels, mappings, overwriteModels, overwriteMappings) => {
            // 处理普通模型选择
            handleModelSelectorConfirm(selectedModels, overwriteModels);

            // 处理映射关系
            if (mappings && mappings.length > 0) {
              if (overwriteMappings) {
                // 覆盖映射模式：清空现有映射，使用新的
                tempSetFieldValue('model_mapping', mappings);
              } else {
                // 追加映射模式：
                const existingMappings = tempFormikValues?.model_mapping || [];
                const existingKeys = new Set(existingMappings.map((item) => item.key));
                const newMappings = mappings.filter((item) => !existingKeys.has(item.key));
                const mergedMappings = [...existingMappings, ...newMappings].map((item, index) => ({
                  ...item,
                  index
                }));
                tempSetFieldValue('model_mapping', mergedMappings);
              }
            }
          }}
          channelValues={tempFormikValues}
          prices={prices}
        />
      </DialogContent>
    </Dialog>
  );
};

export default EditModal;

EditModal.propTypes = {
  open: PropTypes.bool,
  channelId: PropTypes.oneOfType([PropTypes.number, PropTypes.string]),
  onCancel: PropTypes.func,
  onOk: PropTypes.func,
  groupOptions: PropTypes.array,
  isTag: PropTypes.bool,
  modelOptions: PropTypes.array,
  prices: PropTypes.array
};
