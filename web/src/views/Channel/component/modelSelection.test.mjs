import assert from 'node:assert/strict';
import test from 'node:test';
import { modelNavigationTarget, modelWindow, normalizeModelSelection } from './modelSelection.mjs';

test('目录对象、渠道对象和自由输入按 ID 去重，并保留未知模型和选择顺序', () => {
  const known = { id: 'known', group: 'catalog' };
  const unknown = { id: 'upstream-only', group: 'upstream' };
  const selected = normalizeModelSelection(
    ['custom', { id: 'known', group: 'other' }, unknown, 'known', 'custom', ''],
    new Map([['known', known]]),
    '自定义'
  );
  assert.deepEqual(selected, [{ id: 'custom', group: '自定义' }, known, unknown]);
  assert.equal(selected[1], known);
  assert.equal(selected[2], unknown);
});

test('无论滚动位置或目录大小如何，窗口有界且不越界', () => {
  for (const count of [0, 1, 10, 900, 3000]) {
    for (const top of [-10, 0, 480, 144000, 999999]) {
      const { start, end } = modelWindow(top, count);
      assert.ok(start >= 0 && end <= count && start <= end);
      assert.ok(end - start <= 14);
    }
  }
  assert.deepEqual(modelWindow(999999, 3000), { start: 2986, end: 3000 });
});

test('导航跨窗口、首尾循环和翻页边界一致，文字编辑保留 Home/End', () => {
  assert.equal(modelNavigationTarget('ArrowDown', -1, 3000, ''), 0);
  assert.equal(modelNavigationTarget('ArrowUp', -1, 3000, ''), 2999);
  assert.equal(modelNavigationTarget('ArrowDown', 2999, 3000, ''), 0);
  assert.equal(modelNavigationTarget('ArrowUp', 0, 3000, ''), 2999);
  assert.equal(modelNavigationTarget('PageUp', 2, 3000, ''), 0);
  assert.equal(modelNavigationTarget('PageDown', 2998, 3000, ''), 2999);
  assert.equal(modelNavigationTarget('End', 0, 3000, ''), 2999);
  assert.equal(modelNavigationTarget('Home', 2999, 3000, ''), 0);
  assert.equal(modelNavigationTarget('End', 0, 3000, '文字'), -1);
  assert.equal(modelNavigationTarget('Backspace', 0, 3000, ''), -1);
  assert.equal(modelNavigationTarget('ArrowDown', -1, 0, ''), -1);
});
