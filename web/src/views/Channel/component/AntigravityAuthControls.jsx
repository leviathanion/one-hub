import PropTypes from 'prop-types';
import { useEffect, useRef, useState } from 'react';
import { Alert, Box, Button, Dialog, DialogActions, DialogContent, DialogTitle, TextField, Typography } from '@mui/material';

import { API } from 'utils/api';
import { copy, showError, showSuccess } from 'utils/common';

export default function AntigravityAuthControls({ channelId, proxy, onStart, onAuthorized }) {
  const active = useRef(true);
  const pending = useRef(false);
  const [submitting, setSubmitting] = useState(false);
  const [session, setSession] = useState(null);
  const [callbackURL, setCallbackURL] = useState('');
  const [confirmRecovery, setConfirmRecovery] = useState(false);

  useEffect(() => {
    active.current = true;
    return () => {
      active.current = false;
    };
  }, []);

  const closeAuthorization = () => {
    if (pending.current) return;
    setSession(null);
    setCallbackURL('');
    setConfirmRecovery(false);
  };

  const startAuthorization = async () => {
    if (pending.current) return;
    pending.current = true;
    setSubmitting(true);
    try {
      const payload = Number(channelId) > 0 ? { channel_id: Number(channelId) } : { proxy: proxy?.trim() || '' };
      const { data } = await API.post('/api/antigravity/oauth/start', payload, { skipErrorNotification: true });
      if (!active.current) return;
      if (!data.success) throw new Error(data.message || '获取授权链接失败');
      const nextSession = { ...data.data, channel_id: Number(channelId) || 0 };
      onStart?.(nextSession);
      setSession(nextSession);
      setCallbackURL('');
    } catch (error) {
      if (active.current) showError(error.message || '获取授权链接失败，请重试。');
    } finally {
      pending.current = false;
      if (active.current) setSubmitting(false);
    }
  };

  const exchangeCode = async () => {
    if (pending.current || !session) return;
    pending.current = true;
    setSubmitting(true);
    setConfirmRecovery(false);
    try {
      const { data } = await API.post(
        '/api/antigravity/oauth/exchange-code',
        { session_id: session.session_id, callback_url: callbackURL.trim() },
        { skipErrorNotification: true }
      );
      if (!active.current) return;
      if (!data.success) throw new Error(data.message || '授权交换失败，请重新开始授权。');
      await onAuthorized(data.data, session);
      if (!active.current) return;
      showSuccess(
        session.operation === 'recover' ? '同账号凭据已恢复，其他未保存的编辑已保留。' : '授权成功，凭据已填入表单，保存后生效。'
      );
    } catch (error) {
      if (active.current) showError(error.message || '授权结果未确认，请重新开始授权。');
    } finally {
      pending.current = false;
      if (active.current) {
        setSubmitting(false);
        // 交换结果不确定时也不能重放已消费的授权会话。
        closeAuthorization();
      }
    }
  };

  const submitCallback = () => {
    if (!callbackURL.trim() || pending.current) return;
    if (session.operation === 'recover') setConfirmRecovery(true);
    else exchangeCode();
  };

  return (
    <Box sx={{ mt: 2, mb: 2 }}>
      <Button variant="outlined" fullWidth disabled={submitting} onClick={startAuthorization}>
        {submitting ? '正在处理授权…' : 'Antigravity OAuth 授权'}
      </Button>
      <Alert severity="info" sx={{ mt: 1 }}>
        授权后请复制浏览器地址栏中的完整回调 URL。新建或更换账号的凭据会先填入当前表单。
      </Alert>
      <Dialog open={Boolean(session)} onClose={closeAuthorization} maxWidth="md" fullWidth aria-labelledby="antigravity-oauth-title">
        <DialogTitle id="antigravity-oauth-title">Antigravity OAuth 授权</DialogTitle>
        <DialogContent>
          <Alert severity="info" sx={{ mb: 2 }}>
            {(Array.isArray(session?.instructions) ? session.instructions.join(' ') : session?.instructions) ||
              '打开授权页面，登录 Google 并同意授权，再将完整回调 URL 粘贴到下方。回调页面无法打开时，也可以复制地址栏中的 URL。'}
          </Alert>
          {session?.operation === 'recover' && (
            <Alert severity="warning" sx={{ mb: 2 }}>
              当前渠道需要恢复凭据。确认后会立即更新同账号凭据，其他表单内容仍需另行保存。
            </Alert>
          )}
          <Box sx={{ display: 'flex', flexWrap: 'wrap', gap: 1, mb: 2 }}>
            <Button variant="contained" component="a" href={session?.auth_url} target="_blank" rel="noopener noreferrer">
              打开 Google 授权页面
            </Button>
            <Button variant="outlined" onClick={() => copy(session.auth_url)}>
              复制授权链接
            </Button>
          </Box>
          <TextField
            id="antigravity-callback-url"
            label="完整回调 URL"
            fullWidth
            multiline
            minRows={3}
            value={callbackURL}
            disabled={submitting}
            onChange={(event) => setCallbackURL(event.target.value)}
            autoComplete="off"
            inputProps={{ spellCheck: false }}
          />
        </DialogContent>
        <DialogActions>
          <Button onClick={closeAuthorization} disabled={submitting}>
            取消
          </Button>
          <Button variant="contained" onClick={submitCallback} disabled={submitting || !callbackURL.trim()}>
            {submitting ? '正在提交…' : '提交回调 URL'}
          </Button>
        </DialogActions>
      </Dialog>
      <Dialog
        open={confirmRecovery}
        onClose={() => setConfirmRecovery(false)}
        aria-labelledby="antigravity-recovery-title"
        aria-describedby="antigravity-recovery-impact"
      >
        <DialogTitle id="antigravity-recovery-title">确认恢复渠道凭据</DialogTitle>
        <DialogContent id="antigravity-recovery-impact">
          <Typography sx={{ mb: 1 }}>确认后将立即恢复当前渠道的同账号凭据，渠道 ID 保持不变。</Typography>
          <Typography sx={{ mb: 1 }}>凭据变更可能影响已有任务、已存储响应、文件和会话等上游资源的后续访问。</Typography>
          <Typography>其他未保存的配置会保留在表单中。取消则保留回调 URL，不提交恢复。</Typography>
        </DialogContent>
        <DialogActions>
          <Button onClick={() => setConfirmRecovery(false)}>取消</Button>
          <Button color="warning" variant="contained" onClick={exchangeCode}>
            确认恢复
          </Button>
        </DialogActions>
      </Dialog>
    </Box>
  );
}

AntigravityAuthControls.propTypes = {
  channelId: PropTypes.oneOfType([PropTypes.number, PropTypes.string]),
  proxy: PropTypes.string,
  onStart: PropTypes.func,
  onAuthorized: PropTypes.func.isRequired
};
