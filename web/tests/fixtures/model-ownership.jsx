import { createRoot } from 'react-dom/client';
import { Provider } from 'react-redux';
import { BrowserRouter } from 'react-router-dom';
import { ThemeProvider, CssBaseline } from '@mui/material';
import { SnackbarProvider } from 'notistack';
import i18n from 'i18next';
import { initReactI18next } from 'react-i18next';
import theme from '../../src/themes';
import { initialState } from '../../src/store/customizationReducer';
import { store } from '../../src/store';
import ModelInfo from '../../src/views/ModelInfo';
import Pricing from '../../src/views/Pricing';
import zh from '../../src/i18n/locales/zh_CN.json';

i18n.use(initReactI18next).init({ lng: 'zh', resources: { zh: { translation: zh } }, interpolation: { escapeValue: false } });
createRoot(document.getElementById('root')).render(
  <Provider store={store}>
    <ThemeProvider theme={theme(initialState)}>
      <CssBaseline />
      <BrowserRouter>
        <SnackbarProvider>{new URLSearchParams(location.search).has('pricing') ? <Pricing /> : <ModelInfo />}</SnackbarProvider>
      </BrowserRouter>
    </ThemeProvider>
  </Provider>
);
