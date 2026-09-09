import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { createRequire } from 'node:module';
import { fileURLToPath } from 'node:url';
import test, { after } from 'node:test';
import { build } from 'esbuild';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { createInstance } from 'i18next';

const require = createRequire(import.meta.url);
const { I18nextProvider } = require('react-i18next');

// 使用已有构建依赖渲染真实组件，不需要浏览器、网络或新增测试框架。
const root = fileURLToPath(new URL('../../../../', import.meta.url));
const bundled = await build({
  absWorkingDir: root,
  stdin: {
    contents: `export { default as Content } from './src/views/Log/component/QuotaWithDetailContent.jsx';
      export { default as LogRow } from './src/views/Log/component/TableRow.jsx';`,
    resolveDir: root
  },
  bundle: true,
  packages: 'external',
  format: 'cjs',
  platform: 'node',
  jsx: 'automatic',
  write: false,
  define: { 'import.meta.env': '{}' }
});
const previousStorage = globalThis.localStorage;
globalThis.localStorage = { getItem: (key) => ({ quota_per_unit: '500000', display_in_currency: 'true' })[key] ?? null };
after(() => {
  if (previousStorage === undefined) delete globalThis.localStorage;
  else globalThis.localStorage = previousStorage;
});
const module = { exports: {} };
new Function('module', 'exports', 'require', bundled.outputFiles[0].text)(module, module.exports, require);
const { Content, LogRow } = module.exports;
const resources = {};
for (const locale of ['zh_CN', 'zh_HK', 'en_US', 'ja_JP']) {
  resources[locale] = {
    translation: JSON.parse(await readFile(new URL('../../../i18n/locales/' + locale + '.json', import.meta.url), 'utf8'))
  };
}

async function render(item, locale = 'zh_CN', Component = Content) {
  const i18n = createInstance();
  await i18n.init({ lng: locale, resources, interpolation: { escapeValue: false } });
  return renderToStaticMarkup(
    createElement(
      I18nextProvider,
      { i18n },
      createElement(Component, {
        item,
        userIsAdmin: false,
        userGroup: {},
        columnVisibility: { detail: true }
      })
    )
  )
    .replace(/<style[^>]*>[\s\S]*?<\/style>/g, '')
    .replace(/<[^>]+>/g, ' ')
    .replace(/\s+/g, ' ');
}

function log() {
  return {
    type: 2,
    quota: 1360230,
    prompt_tokens: 272001,
    completion_tokens: 10,
    metadata: {
      price_type: 'tokens',
      group_ratio: 0.5,
      input_ratio: 10,
      output_ratio: 45,
      price_version: 4,
      effective_service_tier: 'fast',
      token_billing: {
        status: 'priceable',
        base_input_ratio: 2.5,
        base_output_ratio: 15,
        input_units: 272001,
        output_units: 10,
        charge: 1360230,
        rules: [
          { kind: 'long_context', id: 'long', when: { input_tokens: { gt: 272000 } }, input_multiplier: 2, output_multiplier: 1.5 },
          { kind: 'service_tier', id: 'priority', when: { service_tier: ['fast'] }, input_multiplier: 2, output_multiplier: 2 }
        ]
      }
    }
  };
}

test('使用项目实际四种语言标识渲染费用详情，无缺失翻译或格式化异常', async () => {
  for (const locale of Object.keys(resources)) {
    const html = await render(log(), locale);
    assert.ok(html.includes(resources[locale].translation.logPage.quotaDetail.actualCharge));
    assert.doesNotMatch(html, /logPage\.|NaN|undefined|Infinity/);
  }
});

test('先展示费用，再说明真正调价原因，最后实扣；不暴露内部版本和公式', async () => {
  const html = await render(log());
  assert.match(html, /输入 272,001 tokens \$5 × 2 × 2 × 0\.5 = \$10 \/ 百万 tokens \$2\.720010/);
  assert.match(html, /输出 10 tokens \$30 × 1\.5 × 2 × 0\.5 = \$45 \/ 百万 tokens/);
  assert.match(html, /fast：输入 ×2/);
  assert.match(html, /超过 272,000 的长上下文门槛/);
  assert.match(html, /优惠 50%/);
  assert.ok(html.indexOf('费用组成') < html.indexOf('价格调整原因'));
  assert.ok(html.indexOf('价格调整原因') < html.indexOf('实扣合计'));
  assert.match(html, /实扣合计 \$2\.720460/);
  assert.doesNotMatch(html, /价格版本|price_version|tier|ceil|综合倍率|最终计算|标准单价/);
});

test('标准价格不展示默认档位、未命中规则和没有变化的乘数', async () => {
  const item = log();
  item.metadata.group_ratio = 1;
  item.metadata.effective_service_tier = 'default';
  item.metadata.token_billing.rules = [{ kind: 'fast_priority', input_multiplier: 1, output_multiplier: 1 }];
  item.metadata.input_ratio = item.metadata.token_billing.base_input_ratio;
  item.metadata.output_ratio = item.metadata.token_billing.base_output_ratio;
  const html = await render(item);
  assert.doesNotMatch(html, /价格调整原因|default|快速模式|× 1|未应用/);
});

test('旧日志只提示未保存调整明细，不用当前档位或合并倍率倒推规则', async () => {
  const item = log();
  delete item.metadata.token_billing;
  const html = await render(item);
  assert.match(html, /这条记录未保存价格调整明细/);
  assert.doesNotMatch(html, /长上下文|快速模式|价格版本|tier/);
});

test('冲突 Token 不声称套用了加价，但仍展示独立工具费用和实扣', async () => {
  const item = log();
  Object.assign(item.metadata.token_billing, { status: 'conflicting_evidence', charge: 0 });
  item.metadata.extra_billing = { web_search: { service_type: 'web_search', price: 0.01, call_count: 2 } };
  item.quota = 5000;
  const html = await render(item);
  assert.match(html, /用量信息不一致，Token 部分未收费/);
  assert.match(html, /联网搜索 2 次 \$0\.01 × 0\.5 = \$0\.005 \/ 次 \$0\.010000/);
  assert.match(html, /Token 实扣: \$0\.000000/);
  assert.match(html, /实扣合计 \$0\.010000/);
  assert.doesNotMatch(html, /长上下文|快速模式/);
});

test('实扣为零时仍显示零，不拿参考金额补上', async () => {
  const item = log();
  item.quota = 0;
  item.metadata.token_billing.charge = 0;
  assert.match(await render(item), /实扣合计 \$0\.000000/);
});

test('92 输入样例：列表显示原价，展开后显示原价乘倍率及本次单价', async () => {
  const item = log();
  Object.assign(item, { prompt_tokens: 92, completion_tokens: 69, quota: 60950 });
  Object.assign(item.metadata, { group_ratio: 1, input_ratio: 100, output_ratio: 750 });
  Object.assign(item.metadata.token_billing, {
    base_input_ratio: 50,
    base_output_ratio: 500,
    input_units: 92,
    output_units: 69,
    charge: 60950,
    rules: [{ kind: 'long_context', input_threshold: 10, input_multiplier: 2, output_multiplier: 1.5 }]
  });
  const summary = await render(item, 'zh_CN', LogRow);
  assert.match(summary, /输入原价：\$100 \/M/);
  assert.match(summary, /输出原价：\$1000 \/M/);
  assert.doesNotMatch(summary, /\$200|\$1500/);
  const detail = await render(item);
  assert.match(detail, /\$100 × 2 = \$200 \/ 百万 tokens/);
  assert.match(detail, /\$1000 × 1\.5 = \$1500 \/ 百万 tokens/);
  assert.match(detail, /实扣合计 \$0\.121900/);
});

test('原价徽标在四种语言下均不乘分组折扣，合法零输入价不会隐藏付费输出价', async () => {
  const item = log();
  for (const locale of Object.keys(resources)) {
    const summary = await render(item, locale, LogRow);
    assert.match(summary, /\$5 \/M/);
    assert.match(summary, /\$30 \/M/);
    assert.doesNotMatch(summary, /logPage\.|NaN|undefined/);
  }
  item.metadata.input_ratio = 0;
  item.metadata.token_billing.base_input_ratio = 0;
  const summary = await render(item, 'zh_CN', LogRow);
  assert.match(summary, /输入原价：\$0 \/M/);
  assert.match(summary, /输出原价：\$30 \/M/);
  assert.doesNotMatch(summary, /免费/);
});

test('缺少原价的旧日志不把调价后价格当成原价，也不构造等式', async () => {
  const item = log();
  delete item.metadata.token_billing;
  const summary = await render(item, 'zh_CN', LogRow);
  assert.match(summary, /输入原价：未记录/);
  assert.doesNotMatch(summary, /\$/);
  const detail = await render(item);
  assert.match(detail, /\$10 \/ 百万 tokens 原价未记录/);
  assert.doesNotMatch(detail, /= \$/);
});

test('按次价格只展示分组倍率；非消费日志仍展示原内容', async () => {
  const item = { type: 2, quota: 1250, metadata: { price_type: 'times', input_ratio: 2.5, group_ratio: 0.5 } };
  assert.match(await render(item, 'zh_CN', LogRow), /原价：\$0\.005 \/ 次/);
  assert.match(await render(item), /\$0\.005 × 0\.5 = \$0\.0025 \/ 次/);
  assert.match(await render({ type: 1, content: '充值成功' }, 'zh_CN', LogRow), /充值成功/);
});

test('分组免费时列表显示免费，展开保留原价乘零及零实扣', async () => {
  const item = log();
  item.metadata.group_ratio = 0;
  item.metadata.token_billing.charge = 0;
  item.quota = 0;
  const summary = await render(item, 'zh_CN', LogRow);
  assert.match(summary, /免费/);
  assert.doesNotMatch(summary, /原价|\$/);
  const detail = await render(item);
  assert.match(detail, /\$5 × 2 × 2 × 0 = \$0 \/ 百万 tokens/);
  assert.match(detail, /实扣合计 \$0\.000000/);
});

test('零实扣消费记录统一显示免费，未记录扣费的日志不推测免费', async () => {
  const cases = [
    { ...log(), quota: 0 },
    { type: 2, quota: 0, metadata: { price_type: 'times', input_ratio: 0 } },
    { type: 2, quota: 0 }
  ];
  for (const locale of Object.keys(resources)) {
    for (const item of cases) {
      const summary = await render(item, locale, LogRow);
      assert.ok(summary.includes(resources[locale].translation.logPage.content.free));
      assert.doesNotMatch(summary, /\$|logPage\./);
    }
  }
  assert.doesNotMatch(await render({ type: 2 }, 'zh_CN', LogRow), /免费/);
  assert.match(await render({ type: 1, quota: 0, content: '充值记录' }, 'zh_CN', LogRow), /充值记录/);
});

test('缺价占位的零不能显示为免费原价，但仅缺用量时仍保留已知原价', async () => {
  const item = log();
  item.metadata.token_billing.status = 'missing_evidence';
  item.metadata.token_billing.charge = 0;
  item.metadata.token_billing.rules = [];
  item.metadata.extra_billing = { web_search: { service_type: 'web_search', price: 0.01, call_count: 1 } };
  item.quota = 2500;
  assert.match(await render(item, 'zh_CN', LogRow), /输入原价：\$5 \/M/);
  Object.assign(item.metadata, { input_ratio: 0, output_ratio: 0, billing_diagnostics: ['price_policy_missing_at_settlement'] });
  Object.assign(item.metadata.token_billing, { base_input_ratio: 0, base_output_ratio: 0 });
  const summary = await render(item, 'zh_CN', LogRow);
  assert.match(summary, /输入原价：未记录/);
  assert.doesNotMatch(summary, /\$0|免费/);
  const detail = await render(item);
  assert.match(detail, /联网搜索 1 次 \$0\.01 × 0\.5 = \$0\.005 \/ 次/);
  assert.match(detail, /实扣合计 \$0\.005000/);
});
