import { memo, useCallback, useRef, useState } from 'react';
import PropTypes from 'prop-types';
import { Autocomplete, Box, Checkbox, Chip, FormControl, FormHelperText, TextField } from '@mui/material';
import { createFilterOptions } from '@mui/material/Autocomplete';
import CheckBoxOutlineBlankIcon from '@mui/icons-material/CheckBoxOutlineBlank';
import CheckBoxIcon from '@mui/icons-material/CheckBox';
import { useTheme } from '@mui/material/styles';
import { useField } from 'formik';
import { copy } from 'utils/common';

const icon = <CheckBoxOutlineBlankIcon fontSize="small" />;
const checkedIcon = <CheckBoxIcon fontSize="small" />;

const filter = createFilterOptions();

// 这些对象在组件外声明，避免每次渲染把新的 sx/options 引用交给 Emotion 与 MUI。
const MODEL_CHIP_SX = {
  maxWidth: '100%',
  height: 'auto',
  margin: '3px',
  '& .MuiChip-label': {
    whiteSpace: 'normal',
    wordBreak: 'break-word',
    padding: '6px 8px',
    lineHeight: 1.4,
    fontWeight: 400
  },
  '& .MuiChip-deleteIcon': {
    margin: '0 5px 0 -6px'
  }
};

const MODEL_AUTOCOMPLETE_SX = {
  '& .MuiAutocomplete-tag': {
    margin: '2px'
  },
  '& .MuiAutocomplete-inputRoot': {
    flexWrap: 'wrap'
  }
};

const getModelLabel = (option) => {
  if (typeof option === 'string') {
    return option;
  }
  if (option.inputValue) {
    return option.inputValue;
  }
  return option.id;
};

const getModelGroup = (option) => option.group;

const renderModelOption = (props, option, { selected }) => (
  <li {...props}>
    <Checkbox icon={icon} checkedIcon={checkedIcon} style={{ marginRight: 8 }} checked={selected} />
    {option.id}
  </li>
);

const normalizeModels = (models) => (Array.isArray(models) ? models : []);

/**
 * 模型选择输入。inputValue 只存在于这里，避免在 EditModal 中输入时重渲染整个弹窗；
 * 已选 Chip 元素按 values.models 的引用缓存，输入框自身输入时跳过全部 Chip 的协调。
 */
function ChannelModelsField({ options, disabled, label, helperText, customGroupLabel }) {
  const theme = useTheme();
  const [field, meta, helpers] = useField('models');
  const [inputValue, setInputValue] = useState('');
  const tagsCacheRef = useRef({ value: null, elements: [] });

  const setModels = useCallback((models) => helpers.setValue(models), [helpers]);

  const handleInputChange = useCallback(
    (event, newInputValue) => {
      if (!newInputValue.includes(',')) {
        setInputValue(newInputValue);
        return;
      }

      const currentModels = normalizeModels(field.value);
      const seenIds = new Set(currentModels.map((model) => model.id));
      const addedModels = [];
      for (const item of newInputValue.split(',')) {
        const id = item.trim();
        if (id !== '' && !seenIds.has(id)) {
          seenIds.add(id);
          addedModels.push({ id, group: customGroupLabel });
        }
      }
      if (addedModels.length > 0) {
        setModels([...currentModels, ...addedModels]);
      }
      setInputValue('');
    },
    [customGroupLabel, field.value, setModels]
  );

  const handleChange = useCallback(
    (event, newValue) => {
      setModels(newValue.map((item) => (typeof item === 'string' ? { id: item, group: customGroupLabel } : item)));
    },
    [customGroupLabel, setModels]
  );

  const handleFilterOptions = useCallback(
    (opts, params) => {
      const filtered = filter(opts, params);
      const isExisting = opts.some((option) => params.inputValue === option.id);
      if (params.inputValue !== '' && !isExisting) {
        filtered.push({ id: params.inputValue, group: customGroupLabel });
      }
      return filtered;
    },
    [customGroupLabel]
  );

  const renderTags = useCallback((tagValue, getTagProps) => {
    const cache = tagsCacheRef.current;
    if (cache.value !== tagValue) {
      cache.value = tagValue;
      cache.elements = normalizeModels(tagValue).map((option, index) => {
        const id = typeof option === 'string' ? option : option.id;
        // getTagProps 每次渲染都是新函数；缓存元素时只保留 key/事件等稳定部分。
        const { key, ...tagProps } = getTagProps({ index });
        return <Chip key={key ?? id} label={id} {...tagProps} onClick={() => copy(id)} sx={MODEL_CHIP_SX} />;
      });
    }
    return cache.elements;
  }, []);

  const renderInput = useCallback(
    (params) => (
      <TextField
        {...params}
        name="models"
        error={Boolean(meta.error)}
        label={label}
        InputProps={{
          ...params.InputProps
        }}
      />
    ),
    [label, meta.error]
  );

  return (
    <FormControl fullWidth sx={{ ...theme.typography.otherInput }}>
      <Box sx={{ position: 'relative' }}>
        <Autocomplete
          multiple
          freeSolo
          disableCloseOnSelect
          id="channel-models-label"
          disabled={disabled}
          options={options}
          value={normalizeModels(field.value)}
          inputValue={inputValue}
          onInputChange={handleInputChange}
          onChange={handleChange}
          renderInput={renderInput}
          groupBy={getModelGroup}
          getOptionLabel={getModelLabel}
          filterOptions={handleFilterOptions}
          renderOption={renderModelOption}
          renderTags={renderTags}
          sx={MODEL_AUTOCOMPLETE_SX}
        />
      </Box>
      {meta.error ? (
        <FormHelperText error id="helper-tex-channel-models-label">
          {meta.error}
        </FormHelperText>
      ) : (
        <FormHelperText id="helper-tex-channel-models-label"> {helperText} </FormHelperText>
      )}
    </FormControl>
  );
}

ChannelModelsField.propTypes = {
  options: PropTypes.array,
  disabled: PropTypes.bool,
  label: PropTypes.string,
  helperText: PropTypes.string,
  customGroupLabel: PropTypes.string
};

export default memo(ChannelModelsField);
