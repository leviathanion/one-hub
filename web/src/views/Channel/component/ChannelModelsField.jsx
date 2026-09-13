import { memo, useCallback, useMemo, useRef, useState } from 'react';
import PropTypes from 'prop-types';
import { Autocomplete, Box, Chip, FormControl, FormHelperText, TextField } from '@mui/material';
import { createFilterOptions } from '@mui/material/Autocomplete';
import { useTheme } from '@mui/material/styles';
import { useEventCallback } from '@mui/material/utils';
import { useField } from 'formik';
import { copy } from 'utils/common';
import VirtualModelListbox, { describeModelGroup, describeModelOption, highlightedModelIndex } from './VirtualModelListbox';
import { EMPTY_MODELS, asModels, getModelId, normalizeModelSelection } from './modelSelection.mjs';

const filter = createFilterOptions();
const getModelGroup = (option) => option.group;
const sameModel = (option, value) => getModelId(option) === getModelId(value);
const MODEL_CHIP_SX = {
  maxWidth: '100%',
  height: 'auto',
  margin: '2px',
  '& .MuiChip-label': {
    whiteSpace: 'normal',
    wordBreak: 'break-word',
    padding: '6px 8px',
    lineHeight: 1.4,
    fontWeight: 400
  },
  '& .MuiChip-deleteIcon': { margin: '0 5px 0 -6px' }
};
const MODEL_AUTOCOMPLETE_SX = { '& .MuiAutocomplete-inputRoot': { flexWrap: 'wrap' } };

const ModelChip = memo(function ModelChip({ label, tagIndex, tabIndex, className, disabled, onRemove }) {
  const handleCopy = useCallback(() => copy(label), [label]);
  const handleDelete = useCallback(() => onRemove(label), [label, onRemove]);
  return (
    <Chip
      label={label}
      data-tag-index={tagIndex}
      tabIndex={tabIndex}
      className={className}
      disabled={disabled}
      onClick={handleCopy}
      onDelete={disabled ? undefined : handleDelete}
      sx={MODEL_CHIP_SX}
    />
  );
});

ModelChip.propTypes = {
  label: PropTypes.string.isRequired,
  tagIndex: PropTypes.number.isRequired,
  tabIndex: PropTypes.number,
  className: PropTypes.string,
  disabled: PropTypes.bool,
  onRemove: PropTypes.func.isRequired
};

const ModelsInput = memo(function ModelsInput({
  models,
  error,
  options,
  disabled,
  label,
  helperText,
  customGroupLabel,
  onChange,
  onRemove
}) {
  const theme = useTheme();
  const [inputValue, setInputValue] = useState('');
  const inputRef = useRef(null);
  const virtualRef = useRef(null);
  const highlightedIdRef = useRef(null);
  const optionsById = useMemo(() => new Map(options.map((option) => [option.id, option])), [options]);
  const filteredOptions = useMemo(() => {
    const matched = filter(options, { inputValue, getOptionLabel: getModelId });
    return inputValue !== '' && !optionsById.has(inputValue) ? [...matched, { id: inputValue, group: customGroupLabel }] : matched;
  }, [options, optionsById, inputValue, customGroupLabel]);
  const filterOptions = useCallback(() => filteredOptions, [filteredOptions]);
  const listboxProps = useMemo(() => ({ inputRef, virtualRef, highlightedIdRef, resetKey: inputValue }), [inputValue]);

  const handleInputChange = useCallback(
    (event, value) => {
      if (disabled) return;
      if (!value.includes(',')) {
        setInputValue(value);
        return;
      }
      const added = value
        .split(',')
        .map((id) => id.trim())
        .filter(Boolean);
      onChange(normalizeModelSelection([...models, ...added], optionsById, customGroupLabel));
      setInputValue('');
    },
    [disabled, models, optionsById, customGroupLabel, onChange]
  );
  const handleChange = useCallback(
    (event, value) => {
      if (!disabled) onChange(normalizeModelSelection(value, optionsById, customGroupLabel));
    },
    [disabled, optionsById, customGroupLabel, onChange]
  );
  const handleKeyDown = useCallback((event) => {
    if (event.nativeEvent?.isComposing || event.which === 229) {
      event.defaultMuiPrevented = true;
      return;
    }
    virtualRef.current?.prepareKeyDown(event);
  }, []);
  const handleHighlight = useCallback((event, option, reason) => {
    highlightedIdRef.current = option?.id ?? null;
    virtualRef.current?.highlight(highlightedModelIndex(inputRef.current), reason);
  }, []);
  const renderTags = useCallback(
    (tagValue, getTagProps) =>
      tagValue.map((model, index) => {
        const { tabIndex, className, disabled: tagDisabled } = getTagProps({ index });
        return (
          <ModelChip
            key={getModelId(model)}
            label={getModelId(model)}
            tagIndex={index}
            tabIndex={tabIndex}
            className={className}
            disabled={tagDisabled}
            onRemove={onRemove}
          />
        );
      }),
    [onRemove]
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
          value={models}
          inputValue={inputValue}
          onInputChange={handleInputChange}
          onChange={handleChange}
          onKeyDown={handleKeyDown}
          onHighlightChange={handleHighlight}
          handleHomeEndKeys={inputValue === ''}
          renderInput={(params) => (
            <TextField
              {...params}
              inputRef={inputRef}
              name="models"
              error={Boolean(error)}
              label={label}
              inputProps={{ ...params.inputProps, 'aria-describedby': 'helper-text-channel-models-label' }}
            />
          )}
          groupBy={getModelGroup}
          getOptionLabel={getModelId}
          isOptionEqualToValue={sameModel}
          filterOptions={filterOptions}
          renderOption={describeModelOption}
          renderGroup={describeModelGroup}
          ListboxComponent={VirtualModelListbox}
          ListboxProps={listboxProps}
          renderTags={renderTags}
          sx={MODEL_AUTOCOMPLETE_SX}
        />
      </Box>
      <FormHelperText error={Boolean(error)} id="helper-text-channel-models-label">
        {error || helperText}
      </FormHelperText>
    </FormControl>
  );
});

ModelsInput.propTypes = {
  models: PropTypes.array.isRequired,
  error: PropTypes.string,
  options: PropTypes.array.isRequired,
  disabled: PropTypes.bool,
  label: PropTypes.string,
  helperText: PropTypes.string,
  customGroupLabel: PropTypes.string,
  onChange: PropTypes.func.isRequired,
  onRemove: PropTypes.func.isRequired
};

// Context 的校验等更新止于适配层；选择器只接收渲染所需的值及稳定事件。
function ChannelModelsField({ options = EMPTY_MODELS, disabled, ...props }) {
  const [field, meta, helpers] = useField('models');
  const handleChange = useEventCallback((value) => {
    if (!disabled) helpers.setValue(value);
  });
  const handleRemove = useEventCallback((id) => {
    if (!disabled) helpers.setValue(asModels(field.value).filter((model) => getModelId(model) !== id));
  });
  return (
    <ModelsInput
      {...props}
      options={options}
      disabled={disabled}
      models={asModels(field.value)}
      error={meta.error}
      onChange={handleChange}
      onRemove={handleRemove}
    />
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
