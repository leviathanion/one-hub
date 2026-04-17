import PropTypes from 'prop-types';
import { Button, Chip, FormControl, InputLabel, OutlinedInput, Stack } from '@mui/material';
import { getSecretOptionPlaceholder, getSecretOptionStatus } from './secretOptionState.mjs';

const statusConfig = {
  configured: {
    color: 'success',
    labelKey: 'setting_index.secretOptions.configured'
  },
  notConfigured: {
    color: 'default',
    labelKey: 'setting_index.secretOptions.notConfigured'
  },
  pendingReplace: {
    color: 'warning',
    labelKey: 'setting_index.secretOptions.pendingReplace'
  },
  pendingClear: {
    color: 'error',
    labelKey: 'setting_index.secretOptions.pendingClear'
  }
};

const SecretOptionField = ({ disabled, id, label, name, onChange, onClear, onReset, placeholder, secretState, t, type = 'password' }) => {
  const status = statusConfig[getSecretOptionStatus(secretState)] || statusConfig.notConfigured;
  const showClearButton = secretState?.configured || secretState?.action === 'clear';

  return (
    <Stack spacing={1}>
      <FormControl fullWidth>
        <InputLabel htmlFor={id}>{label}</InputLabel>
        <OutlinedInput
          id={id}
          name={name}
          type={type}
          value={secretState?.draft || ''}
          onChange={onChange}
          label={label}
          placeholder={getSecretOptionPlaceholder(t, secretState, placeholder)}
          disabled={disabled}
          inputProps={{ autoComplete: 'new-password' }}
        />
      </FormControl>
      <Stack direction="row" spacing={1} alignItems="center" useFlexGap flexWrap="wrap">
        <Chip size="small" color={status.color} label={t(status.labelKey)} />
        {showClearButton && (
          <Button
            size="small"
            color={secretState?.action === 'clear' ? 'inherit' : 'error'}
            onClick={secretState?.action === 'clear' ? onReset : onClear}
            disabled={disabled}
          >
            {secretState?.action === 'clear' ? t('setting_index.secretOptions.undoClear') : t('setting_index.secretOptions.clear')}
          </Button>
        )}
      </Stack>
    </Stack>
  );
};

SecretOptionField.propTypes = {
  disabled: PropTypes.bool,
  id: PropTypes.string.isRequired,
  label: PropTypes.string.isRequired,
  name: PropTypes.string.isRequired,
  onChange: PropTypes.func.isRequired,
  onClear: PropTypes.func.isRequired,
  onReset: PropTypes.func.isRequired,
  placeholder: PropTypes.string,
  secretState: PropTypes.shape({
    action: PropTypes.string,
    configured: PropTypes.bool,
    draft: PropTypes.string
  }),
  t: PropTypes.func.isRequired,
  type: PropTypes.string
};

export default SecretOptionField;
