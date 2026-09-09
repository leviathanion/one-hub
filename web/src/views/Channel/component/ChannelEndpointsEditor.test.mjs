import assert from 'node:assert/strict';
import test from 'node:test';
import { readFile } from 'node:fs/promises';
import { createRequire } from 'node:module';
import { fileURLToPath } from 'node:url';
import { build } from 'esbuild';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { createInstance } from 'i18next';

const require = createRequire(import.meta.url);
const { I18nextProvider } = require('react-i18next');
const root = fileURLToPath(new URL('../../../../', import.meta.url));
const compiled = await build({
  absWorkingDir: root,
  stdin: { contents: "export {default} from './src/views/Channel/component/ChannelEndpointsEditor.jsx';", resolveDir: root },
  bundle: true,
  packages: 'external',
  format: 'cjs',
  platform: 'node',
  jsx: 'automatic',
  write: false
});
const module = { exports: {} };
new Function('module', 'exports', 'require', compiled.outputFiles[0].text)(module, module.exports, require);
const Editor = module.exports.default;
const messages = JSON.parse(await readFile(new URL('../../../i18n/locales/zh_CN.json', import.meta.url)));
const i18n = createInstance();
await i18n.init({ lng: 'zh_CN', resources: { zh_CN: { translation: messages } }, interpolation: { escapeValue: false } });
const definitions = [
  { id: 'openai.responses', label: 'Responses', group: 'OpenAI API', default_path: '/v1/responses' },
  { id: 'anthropic.messages', label: 'Messages', group: 'Claude API', default_path: '/v1/messages' }
];
const plugin = {
  endpoints: {
    'openai.responses': { enabled: true, upstream_url: '' },
    'anthropic.messages': { enabled: false, upstream_url: '/retained/messages' }
  }
};
const render = (disabled = false) =>
  renderToStaticMarkup(
    createElement(
      I18nextProvider,
      { i18n },
      createElement(Editor, {
        definitions,
        plugin,
        baseURL: 'https://example.com/root',
        disabled,
        onChange() {}
      })
    )
  );

test('按协议分组，开关具有接口名称，关闭地址保留且不可编辑', () => {
  const html = render();
  assert.match(html, /<section[^>]*aria-label="OpenAI API"/);
  assert.match(html, /<section[^>]*aria-label="Claude API"/);
  const inputs = [...html.matchAll(/<input\b[^>]*>/g)].map(([input]) => input);
  const enable = inputs.find((input) => input.includes('aria-label="启用 Responses"'));
  assert.match(enable, /checked=""/);
  assert.doesNotMatch(enable, /disabled=""/);
  const messagesInput = inputs.find((input) => input.includes('aria-label="Messages 上游地址"'));
  assert.match(messagesInput, /disabled=""/);
  assert.match(messagesInput, /value="\/retained\/messages"/);
  assert.match(messagesInput, /aria-describedby="channel-endpoint-anthropic.messages-helper-text"/);
  assert.match(html, /https:\/\/example.com\/root\/v1\/responses/);
});

test('标签锁定时开关和地址都禁用，同行容器允许地址收缩', () => {
  const html = render(true);
  for (const [input] of html.matchAll(/<input\b[^>]*>/g)) assert.match(input, /disabled=""/);
  // SSR 输出实际样式契约，确保窄屏仍为横向排列且地址可收缩。
  assert.match(html, /display:flex;[^}]*align-items:flex-start;gap:8px/);
  assert.match(html, /min-width:0/);
});
