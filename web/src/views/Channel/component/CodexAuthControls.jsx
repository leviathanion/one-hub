import PropTypes from 'prop-types';
import { useRef, useState } from 'react';

import { API } from 'utils/api';
import { copy, showError, showSuccess } from 'utils/common';
import { Alert, Box, Button, Dialog, DialogActions, DialogContent, DialogTitle, TextField, Typography } from '@mui/material';
import { Icon } from '@iconify/react';

export default function CodexAuthControls({ channelId, proxy, currentName, onCredentials, onSuggestedName }) {
  const authFileInputRef = useRef(null);
  const [authFileImporting, setAuthFileImporting] = useState(false);
  const [oauthVisible, setOauthVisible] = useState(false);
  const [authURL, setAuthURL] = useState('');
  const [sessionId, setSessionId] = useState('');
  const [authCode, setAuthCode] = useState('');
  const [submitting, setSubmitting] = useState(false);

  const handleOAuth = async () => {
    const trimmedProxy = proxy ? proxy.trim() : '';
    const payload = {
      proxy: trimmedProxy
    };

    if (Number(channelId) > 0) {
      payload.channel_id = Number(channelId);
    }

    try {
      setSubmitting(true);
      const res = await API.post('/api/codex/oauth/start', payload);

      if (!res.data.success) {
        showError(res.data.message || 'Failed to get authorization link');
        setSubmitting(false);
        return;
      }

      const nextAuthURL = res.data.data.auth_url;
      const nextSessionId = res.data.data.session_id;

      setAuthURL(nextAuthURL);
      setSessionId(nextSessionId);
      setOauthVisible(true);
      setSubmitting(false);

      window.open(nextAuthURL, '_blank');
    } catch (error) {
      showError('Failed to get authorization link: ' + (error.message || error));
      setSubmitting(false);
    }
  };

  const handleSubmitCode = async () => {
    if (!authCode || authCode.trim() === '') {
      showError('Please enter the authorization code or callback URL');
      return;
    }

    try {
      setSubmitting(true);
      const res = await API.post('/api/codex/oauth/exchange-code', {
        session_id: sessionId,
        callback_url: authCode.trim()
      });

      if (!res.data.success) {
        showError(res.data.message || 'Failed to exchange authorization code');
        setSubmitting(false);
        return;
      }

      onCredentials(res.data.data.credentials);
      showSuccess('OAuth successful. Credentials have been filled in.');

      handleCancelOAuth();
    } catch (error) {
      showError('Failed to exchange authorization code: ' + (error.message || error));
      setSubmitting(false);
    }
  };

  const handleCancelOAuth = () => {
    setOauthVisible(false);
    setAuthURL('');
    setSessionId('');
    setAuthCode('');
    setSubmitting(false);
  };

  const handleAuthFileImport = async (event) => {
    const input = event.target;
    const [file] = Array.from(input.files || []);

    if (!file) {
      return;
    }

    setAuthFileImporting(true);

    try {
      const formData = new FormData();
      formData.append('file', file);

      const res = await API.post('/api/codex/auth-files/parse', formData, {
        headers: {
          'Content-Type': 'multipart/form-data'
        }
      });

      if (!res.data.success) {
        showError(res.data.message || 'Failed to import auth file');
        return;
      }

      const { credentials, suggested_name: suggestedName } = res.data.data;
      onCredentials(credentials);
      if (!currentName && suggestedName && typeof onSuggestedName === 'function') {
        onSuggestedName(suggestedName);
      }
      showSuccess('Auth file imported successfully');
    } catch (error) {
      showError(error.message || error);
    } finally {
      input.value = '';
      setAuthFileImporting(false);
    }
  };

  return (
    <Box sx={{ mt: 2, mb: 2 }}>
      <input ref={authFileInputRef} hidden type="file" accept=".json,application/json" onChange={handleAuthFileImport} />
      <Box sx={{ display: 'flex', gap: 1, flexWrap: 'wrap', mb: 2 }}>
        <Button
          variant="outlined"
          color="secondary"
          disabled={authFileImporting}
          onClick={() => authFileInputRef.current?.click()}
          startIcon={authFileImporting ? null : <Icon icon="solar:upload-bold-duotone" />}
        >
          {authFileImporting ? 'Importing auth file...' : 'Import Auth File'}
        </Button>
      </Box>
      <Button
        variant="outlined"
        color="primary"
        fullWidth
        disabled={submitting}
        onClick={handleOAuth}
        startIcon={submitting ? null : <Icon icon="simple-icons:openai" />}
      >
        {submitting ? 'Getting authorization link...' : 'OAuth Authorization'}
      </Button>
      <Alert severity="info" sx={{ mt: 1 }}>
        After authorization, copy the full callback URL and paste it below.
      </Alert>

      <Dialog open={oauthVisible} onClose={handleCancelOAuth} maxWidth="md" fullWidth>
        <DialogTitle>Codex OAuth</DialogTitle>
        <DialogContent>
          <Box sx={{ mb: 2 }}>
            <Alert severity="info" sx={{ mb: 2 }}>
              <Typography variant="body2" component="div">
                <strong>Steps:</strong>
                <ol style={{ margin: '8px 0', paddingLeft: '20px' }}>
                  <li>Open the authorization page.</li>
                  <li>Sign in to OpenAI and approve access.</li>
                  <li>Copy the full callback URL from the browser.</li>
                  <li>Paste the URL below and submit.</li>
                </ol>
              </Typography>
            </Alert>

            <Box sx={{ mb: 2, display: 'flex', gap: 1 }}>
              <Button
                variant="contained"
                color="primary"
                fullWidth
                onClick={() => window.open(authURL, '_blank')}
                startIcon={<Icon icon="mdi:open-in-new" />}
              >
                Open Authorization Page
              </Button>
              <Button
                variant="outlined"
                color="secondary"
                onClick={() => {
                  copy(authURL);
                }}
                startIcon={<Icon icon="mdi:content-copy" />}
                sx={{ minWidth: '120px' }}
              >
                Copy Link
              </Button>
            </Box>

            <TextField
              fullWidth
              label="Callback URL or Authorization Code"
              placeholder="Paste the full callback URL here"
              value={authCode}
              onChange={(e) => setAuthCode(e.target.value)}
              multiline
              rows={3}
              variant="outlined"
            />
          </Box>
        </DialogContent>
        <DialogActions>
          <Button onClick={handleCancelOAuth} disabled={submitting}>
            Cancel
          </Button>
          <Button onClick={handleSubmitCode} variant="contained" color="primary" disabled={submitting || !authCode}>
            {submitting ? 'Submitting...' : 'Submit'}
          </Button>
        </DialogActions>
      </Dialog>
    </Box>
  );
}

CodexAuthControls.propTypes = {
  channelId: PropTypes.oneOfType([PropTypes.number, PropTypes.string]),
  proxy: PropTypes.string,
  currentName: PropTypes.string,
  onCredentials: PropTypes.func.isRequired,
  onSuggestedName: PropTypes.func
};
