import { useState } from 'react';
import { createRoot } from 'react-dom/client';
import { useTranslation } from 'react-i18next';
import { ThemeProvider, CssBaseline, Button } from '@mui/material';
import theme from '../../src/themes';
import { initialState } from '../../src/store/customizationReducer';
import { SnackbarProvider } from 'notistack';
import { CheckUpdates } from '../../src/views/Pricing/component/CheckUpdates';
import i18n from '../../src/i18n/i18n';
import i18nList from '../../src/i18n/i18nList';

i18n.changeLanguage(new URLSearchParams(location.search).get('lang') || 'en_US');
if (new URLSearchParams(location.search).has('default-url')) localStorage.removeItem('oneapi_price_update_url');
else localStorage.setItem('oneapi_price_update_url', '/catalog');
function App() {
  const [open, setOpen] = useState(true);
  const { i18n } = useTranslation();
  return (
    <ThemeProvider theme={theme(initialState)}>
      <CssBaseline />
      <SnackbarProvider>
        <label htmlFor="fixture-language">测试语言</label>
        <select id="fixture-language" value={i18n.language} onChange={(event) => i18n.changeLanguage(event.target.value)}>
          {i18nList.map(({ lng, name }) => (
            <option key={lng} value={lng}>
              {name}
            </option>
          ))}
        </select>
        <Button onClick={() => setOpen(true)}>Open sync</Button>
        <CheckUpdates open={open} onCancel={() => setOpen(false)} onOk={() => setOpen(false)} />
      </SnackbarProvider>
    </ThemeProvider>
  );
}
createRoot(document.getElementById('root')).render(<App />);
