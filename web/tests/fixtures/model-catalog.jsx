import { createRoot } from 'react-dom/client';
import { Provider } from 'react-redux';
import { ThemeProvider, CssBaseline } from '@mui/material';
import { SnackbarProvider } from 'notistack';
import i18n from 'i18next';
import { initReactI18next } from 'react-i18next';
import theme from '../../src/themes';
import { initialState } from '../../src/store/customizationReducer';
import { store } from '../../src/store';
import ModelInfo from '../../src/views/ModelInfo';
import ModelPrice from '../../src/views/ModelPrice';
import zh from '../../src/i18n/locales/zh_CN.json';

i18n.use(initReactI18next).init({ lng: 'zh', resources: { zh: { translation: zh } }, interpolation: { escapeValue: false } });
createRoot(document.getElementById('root')).render(
  <Provider store={store}>
    <ThemeProvider theme={theme(initialState)}>
      <CssBaseline />
      <SnackbarProvider>{new URLSearchParams(location.search).has('price') ? <ModelPrice /> : <ModelInfo />}</SnackbarProvider>
    </ThemeProvider>
  </Provider>
);
