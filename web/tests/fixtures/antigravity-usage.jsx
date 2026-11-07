import { useState } from 'react';
import { createRoot } from 'react-dom/client';
import i18next from 'i18next';
import { I18nextProvider, initReactI18next } from 'react-i18next';
import zhCN from '../../src/i18n/locales/zh_CN.json';
import AntigravityUsageDialog from '../../src/views/Channel/component/AntigravityUsageDialog';

const i18n = i18next.createInstance();
await i18n.use(initReactI18next).init({
  lng: 'zh_CN',
  resources: { zh_CN: { translation: zhCN } },
  interpolation: { escapeValue: false }
});

function App() {
  const [record, setRecord] = useState(null);
  return (
    <I18nextProvider i18n={i18n}>
      <button id="open-7" onClick={() => setRecord({ id: 7, name: '渠道七' })}>
        查看渠道七
      </button>
      <button id="open-8" onClick={() => setRecord({ id: 8, name: '渠道八' })}>
        查看渠道八
      </button>
      {record ? <AntigravityUsageDialog key={record.id} record={record} onClose={() => setRecord(null)} /> : null}
    </I18nextProvider>
  );
}

createRoot(document.getElementById('root')).render(<App />);
