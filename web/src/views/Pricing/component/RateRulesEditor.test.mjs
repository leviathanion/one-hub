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
  stdin: { contents: "export {default} from './src/views/Pricing/component/RateRulesEditor.jsx';", resolveDir: root },
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
const rule = (id, when) => ({ id, when, multipliers: { all: 0.5 } });
const value = {
  version: 2,
  service_tier: [rule('tier', { service_tier: ['flex'] })],
  speed: [rule('speed', { speed: ['fast'] })],
  long_context: [rule('long', { input_tokens: { gt: 200000 } })],
  schedule: {
    timezone: 'Asia/Shanghai',
    rules: [rule('weekend', { weekdays: [6, 7] }), rule('night', { time: { start: '23:00', end: '07:00' } })]
  }
};
const html = renderToStaticMarkup(createElement(I18nextProvider, { i18n }, createElement(Editor, { value, onChange() {} })));
function section(label) {
  const match = html.match(new RegExp(`<section[^>]*aria-label="${label}"[^>]*>([\\s\\S]*?)</section>`));
  assert.ok(match, label);
  return match[1];
}

test('服务档位和速度只展示自己的条件，没有名称或排序', () => {
  const tiers = section('实际服务档位（service_tier）');
  assert.match(tiers, /flex/);
  assert.match(tiers, /实际服务档位（service_tier）/);
  assert.doesNotMatch(tiers, /实际速度|完整输入 Token|星期|时区|优先级|规则名称/);
  const speed = section('实际速度（speed）');
  assert.match(speed, /fast/);
  assert.match(speed, /实际速度（speed）/);
  assert.doesNotMatch(speed, /实际服务档位|完整输入 Token|星期|时区|优先级|规则名称/);
  assert.doesNotMatch(html, /规则名称/);
});

test('长上下文仅显示长度与可选适用速度，时区与优先级仅在时间配置中', () => {
  const context = section('长上下文');
  assert.match(context, /完整输入 Token 大于/);
  assert.match(context, /适用速度（speed，可选）/);
  assert.doesNotMatch(context, /实际服务档位|星期|时区|提高优先级|降低优先级/);
  const schedule = section('时间折扣');
  assert.match(schedule, /role="button"[^>]*>[\s\S]*?时间折扣/);
  assert.match(schedule, /role="region"[\s\S]*?Asia\/Shanghai/);
  assert.match(schedule, /提高优先级/);
  assert.match(schedule, /降低优先级/);
  assert.match(schedule, /周六/);
  assert.doesNotMatch(schedule, /实际服务档位|实际速度|完整输入 Token/);
});
