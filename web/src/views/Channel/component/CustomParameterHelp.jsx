import { Box, Button, Link, Typography } from '@mui/material';
import { enqueueSnackbar } from 'notistack';
import { useTranslation } from 'react-i18next';
import { getSnackbarOptions, showSuccess } from 'utils/common';

const basicExamples = [
  { key: 'default', rules: ['defaultRule'], json: '{\n  "temperature": 0.7\n}' },
  { key: 'overwrite', rules: ['overwriteRule'], json: '{\n  "overwrite": true,\n  "temperature": 0.7\n}' },
  { key: 'remove', rules: ['removeRule'], json: '{\n  "remove_params": ["temperature", "metadata.internal"]\n}' }
];

const advancedExamples = [
  {
    key: 'perModel',
    rules: ['modelRule'],
    json: '{\n  "per_model": true,\n  "overwrite": true,\n  "upstream-model": {\n    "temperature": 0.7\n  }\n}'
  },
  { key: 'preAdd', rules: ['stageRule'], json: '{\n  "pre_add": true,\n  "remove_params": ["temperature"]\n}' }
];

export default function CustomParameterHelp() {
  const { t } = useTranslation();
  const help = (key, options) => t(`channel_edit.customParameters.${key}`, options);
  const copyExample = async (json) => {
    try {
      await navigator.clipboard.writeText(json);
      showSuccess(help('copied'));
    } catch {
      enqueueSnackbar(help('copyFailed'), getSnackbarOptions('ERROR'));
    }
  };

  const renderExample = ({ key, rules, json }) => (
    <Box key={key} sx={{ mt: 2 }}>
      <Box sx={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 1 }}>
        <Typography variant="subtitle2">{help(`${key}Title`)}</Typography>
        <Button
          type="button"
          size="small"
          sx={{ flexShrink: 0 }}
          onClick={() => copyExample(json)}
          aria-label={help('copyExample', { name: help(`${key}Title`) })}
        >
          {help('copy')}
        </Button>
      </Box>
      <Box
        component="pre"
        sx={{
          m: 0,
          p: 1.5,
          borderRadius: 1,
          bgcolor: 'action.hover',
          typography: 'body2',
          whiteSpace: 'pre-wrap',
          overflowWrap: 'anywhere'
        }}
        tabIndex={0}
        aria-label={help(`${key}Title`)}
      >
        <code>{json}</code>
      </Box>
      {rules.map((rule) => (
        <Typography key={rule} variant="body2" sx={{ mt: 1, overflowWrap: 'anywhere' }}>
          {help(rule)}
        </Typography>
      ))}
    </Box>
  );

  return (
    <Box component="details" sx={{ mt: 1, minWidth: 0, color: 'text.secondary' }}>
      <Box component="summary" sx={{ cursor: 'pointer', typography: 'body2', color: 'primary.main', py: 1 }}>
        {help('title')}
      </Box>
      <Typography variant="body2" sx={{ mb: 1 }}>
        {help('intro')}
      </Typography>
      <Link
        href="https://github.com/leviathanion/one-hub/blob/main/docs/use/channel-parameters.md"
        target="_blank"
        rel="noopener noreferrer"
        variant="body2"
      >
        {help('docs')}
      </Link>
      {basicExamples.map(renderExample)}
      <Typography variant="body2" sx={{ mt: 2 }}>
        {help('scopeHint')}
      </Typography>
      <Box component="details" sx={{ mt: 1 }}>
        <Box component="summary" sx={{ cursor: 'pointer', typography: 'body2', color: 'primary.main', py: 1 }}>
          {help('advancedTitle')}
        </Box>
        {advancedExamples.map(renderExample)}
        <Box component="ul" sx={{ pl: 3, typography: 'body2', '& li': { mb: 1 }, overflowWrap: 'anywhere' }}>
          {['nestedRule', 'orderRule', 'protectedRule', 'scopeRule'].map((key) => (
            <li key={key}>{help(key)}</li>
          ))}
        </Box>
      </Box>
    </Box>
  );
}
