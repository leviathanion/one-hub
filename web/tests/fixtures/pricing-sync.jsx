import { useState } from 'react';
import { createRoot } from 'react-dom/client';
import i18n from 'i18next';
import { initReactI18next } from 'react-i18next';
import { SnackbarProvider } from 'notistack';
import { CheckUpdates } from '../../src/views/Pricing/component/CheckUpdates';
import en from '../../src/i18n/locales/en_US.json';
import { pricingTranslations } from '../../src/locales/pricing';

i18n.use(initReactI18next).init({
  lng: 'en',
  resources: { en: { translation: { ...en, ...pricingTranslations.en } } },
  interpolation: { escapeValue: false }
});
localStorage.setItem('oneapi_price_update_url', '/catalog');
function App() {
  const [open, setOpen] = useState(true);
  return (
    <SnackbarProvider>
      <button onClick={() => setOpen(true)}>Open sync</button>
      <CheckUpdates open={open} onCancel={() => setOpen(false)} onOk={() => setOpen(false)} />
    </SnackbarProvider>
  );
}
createRoot(document.getElementById('root')).render(<App />);
