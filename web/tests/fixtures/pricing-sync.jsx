import { useState } from 'react';
import { createRoot } from 'react-dom/client';
import i18n from 'i18next';
import { initReactI18next } from 'react-i18next';
import { ThemeProvider, CssBaseline, Button } from '@mui/material';
import theme from '../../src/themes';
import { initialState } from '../../src/store/customizationReducer';
import { SnackbarProvider } from 'notistack';
import { CheckUpdates } from '../../src/views/Pricing/component/CheckUpdates';
import en from '../../src/i18n/locales/en_US.json';
import zh from '../../src/i18n/locales/zh_CN.json';
import { pricingTranslations } from '../../src/locales/pricing';

i18n.use(initReactI18next).init({
  lng: new URLSearchParams(location.search).get('lang') || 'en',
  resources: { en: { translation: { ...en, ...pricingTranslations.en } }, zh: { translation: { ...zh, ...pricingTranslations.zh } } },
  interpolation: { escapeValue: false }
});
if (new URLSearchParams(location.search).has('default-url')) localStorage.removeItem('oneapi_price_update_url');
else localStorage.setItem('oneapi_price_update_url', '/catalog');
function App() {
  const [open, setOpen] = useState(true);
  return (
    <ThemeProvider theme={theme(initialState)}>
      <CssBaseline />
      <SnackbarProvider>
        <Button onClick={() => setOpen(true)}>Open sync</Button>
        <CheckUpdates ownedby={[{ value: 1, label: 'OpenAI' }]} open={open} onCancel={() => setOpen(false)} onOk={() => setOpen(false)} />
      </SnackbarProvider>
    </ThemeProvider>
  );
}
createRoot(document.getElementById('root')).render(<App />);
