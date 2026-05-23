import PropTypes from 'prop-types';
import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';

import { API } from 'utils/api';
import { showError, showSuccess } from 'utils/common';
import { Button, Dialog, DialogActions, DialogContent, DialogTitle, TextField } from '@mui/material';

import CodexAuthControls from './CodexAuthControls';
import { isCodexChannel } from './codexUsage';

export default function TagChannelCreateDialog({ open, tag, representative, onClose, onCreated }) {
  const { t } = useTranslation();
  const [name, setName] = useState('');
  const [key, setKey] = useState('');
  const [submitting, setSubmitting] = useState(false);
  const isCodex = isCodexChannel(representative);

  useEffect(() => {
    if (open) {
      setName('');
      setKey('');
      setSubmitting(false);
    }
  }, [open]);

  const handleSubmit = async () => {
    if (!key.trim()) {
      showError(t('channel_row.keyRequired'));
      return;
    }

    try {
      setSubmitting(true);
      const res = await API.post(`/api/channel_tag/${encodeURIComponent(tag)}/channel`, {
        name: name.trim(),
        key
      });

      const { success, message, data } = res.data;
      if (!success) {
        showError(message || t('channel_edit.addError'));
        return;
      }

      showSuccess(t('channel_edit.addSuccess'));
      if (typeof onCreated === 'function') {
        onCreated(data);
      }
      onClose();
    } catch (error) {
      showError(error.response?.data?.message || error.message || t('channel_edit.addError'));
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <Dialog open={open} onClose={onClose} fullWidth maxWidth="md">
      <DialogTitle>{t('channel_row.addTagChannel')}</DialogTitle>
      <DialogContent>
        <TextField
          autoFocus
          margin="dense"
          id="tag-channel-name"
          label={t('channel_index.channelName')}
          type="text"
          fullWidth
          variant="outlined"
          value={name}
          onChange={(e) => setName(e.target.value)}
          sx={{ mb: 2, mt: 1 }}
        />
        <TextField
          margin="dense"
          id="tag-channel-key"
          label={t('channel_row.key')}
          type="text"
          fullWidth
          multiline
          minRows={isCodex ? 5 : 3}
          maxRows={isCodex ? 12 : undefined}
          variant="outlined"
          value={key}
          onChange={(e) => setKey(e.target.value)}
        />
        {isCodex && (
          <CodexAuthControls
            proxy={representative?.proxy || ''}
            currentName={name}
            onCredentials={setKey}
            onSuggestedName={(suggestedName) => {
              if (!name.trim()) {
                setName(suggestedName);
              }
            }}
          />
        )}
      </DialogContent>
      <DialogActions>
        <Button onClick={onClose} disabled={submitting}>
          {t('common.cancel')}
        </Button>
        <Button variant="contained" color="primary" onClick={handleSubmit} disabled={submitting}>
          {t('common.submit')}
        </Button>
      </DialogActions>
    </Dialog>
  );
}

TagChannelCreateDialog.propTypes = {
  open: PropTypes.bool,
  tag: PropTypes.string,
  representative: PropTypes.object,
  onClose: PropTypes.func,
  onCreated: PropTypes.func
};
