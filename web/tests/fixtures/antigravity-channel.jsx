import { useState } from 'react';
import { createRoot } from 'react-dom/client';
import { ThemeProvider, CssBaseline } from '@mui/material';
import { SnackbarProvider } from 'notistack';
import { Provider } from 'react-redux';
import { store } from '../../src/store';
import theme from '../../src/themes';
import { initialState } from '../../src/store/customizationReducer';
import EditModal from '../../src/views/Channel/component/EditModal';
import TagChannelCreateDialog from '../../src/views/Channel/component/TagChannelCreateDialog';
import i18n from '../../src/i18n/i18n';

i18n.changeLanguage('zh_CN');
const mode = new URLSearchParams(location.search).get('mode') || 'edit';

function App() {
  const [open, setOpen] = useState(true);
  const [channelId, setChannelId] = useState(42);
  return (
    <ThemeProvider theme={theme(initialState)}>
      <CssBaseline />
      <SnackbarProvider>
        <button onClick={() => setOpen(true)}>重新打开测试表单</button>
        <button onClick={() => setChannelId((id) => (id === 42 ? 43 : 42))}>切换测试渠道</button>
        {mode === 'tag-create' ? (
          <TagChannelCreateDialog
            open={open}
            tag="测试标签"
            representative={{ type: 102, proxy: 'http://proxy.example' }}
            onClose={() => setOpen(false)}
          />
        ) : (
          <EditModal
            open={open}
            channelId={mode === 'create' ? 0 : mode === 'tag-edit' ? '测试标签' : channelId}
            isTag={mode === 'tag-edit'}
            groupOptions={['default']}
            modelOptions={[{ id: 'gemini-test', group: '测试' }]}
            prices={[]}
            onCancel={() => setOpen(false)}
            onOk={() => setOpen(false)}
          />
        )}
      </SnackbarProvider>
    </ThemeProvider>
  );
}

createRoot(document.getElementById('root')).render(
  <Provider store={store}>
    <App />
  </Provider>
);
