import PropTypes from 'prop-types';
import { Box, FormControlLabel, Switch, TextField, Typography } from '@mui/material';
import { useTranslation } from 'react-i18next';
import { previewEndpoint, updateEndpoint } from '../type/endpoints.mjs';

export default function ChannelEndpointsEditor({ definitions, plugin, baseURL, disabled, onChange }) {
  const { t } = useTranslation();
  const groups = [...new Set(definitions.map((definition) => definition.group))];
  return groups.map((group) => (
    <Box component="section" key={group} aria-label={group} sx={{ border: 1, borderColor: 'divider', borderRadius: 2, p: 2, my: 2 }}>
      <Typography variant="h3" sx={{ mb: 1 }}>
        {group}
      </Typography>
      <Typography variant="caption" color="text.secondary">
        {t('channel_edit.endpoints.description')}
      </Typography>
      {definitions
        .filter((definition) => definition.group === group)
        .map((definition) => {
          const setting = plugin?.endpoints?.[definition.id];
          const enabled = setting?.enabled === true;
          const preview = previewEndpoint(baseURL, setting, definition);
          const inputID = `channel-endpoint-${definition.id}`;
          return (
            <Box key={definition.id} sx={{ mt: 2 }}>
              <Typography variant="body2" sx={{ display: { xs: 'block', sm: 'none' }, mb: 1 }}>
                {definition.label}
              </Typography>
              <Box sx={{ display: 'flex', alignItems: 'flex-start', gap: 1 }}>
                <FormControlLabel
                  sx={{ m: 0, width: { xs: 58, sm: 210 }, flexShrink: 0, alignItems: 'flex-start' }}
                  control={
                    <Switch
                      checked={enabled}
                      disabled={disabled}
                      inputProps={{ 'aria-label': t('channel_edit.endpoints.enable', { name: definition.label }) }}
                      onChange={(event) => onChange(updateEndpoint(plugin, definition.id, { enabled: event.target.checked }))}
                    />
                  }
                  label={
                    <Typography variant="body2" sx={{ pt: 1, display: { xs: 'none', sm: 'block' } }}>
                      {definition.label}
                    </Typography>
                  }
                />
                <TextField
                  id={inputID}
                  fullWidth
                  size="small"
                  sx={{ minWidth: 0 }}
                  disabled={disabled || !enabled}
                  label={t('channel_edit.endpoints.upstreamURL')}
                  inputProps={{ 'aria-label': t('channel_edit.endpoints.address', { name: definition.label }) }}
                  value={setting?.upstream_url ?? ''}
                  placeholder={definition.default_path}
                  onChange={(event) => onChange(updateEndpoint(plugin, definition.id, { upstream_url: event.target.value }))}
                  helperText={
                    enabled
                      ? preview
                        ? t('channel_edit.endpoints.effectiveURL', { url: preview })
                        : t('channel_edit.endpoints.missingBaseURL')
                      : t('channel_edit.endpoints.disabled')
                  }
                  FormHelperTextProps={{ sx: { overflowWrap: 'anywhere', whiteSpace: 'pre-wrap' } }}
                />
              </Box>
            </Box>
          );
        })}
      {group === 'OpenAI API' && (
        <Typography variant="caption" color="text.secondary" sx={{ display: 'block', mt: 2 }}>
          {t('channel_edit.endpoints.responsesScope')}
        </Typography>
      )}
    </Box>
  ));
}

ChannelEndpointsEditor.propTypes = {
  definitions: PropTypes.array.isRequired,
  plugin: PropTypes.object,
  baseURL: PropTypes.string,
  disabled: PropTypes.bool,
  onChange: PropTypes.func.isRequired
};
