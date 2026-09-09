import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';

const source = await readFile(new URL('./EditModal.jsx', import.meta.url), 'utf8');

test('Other JSON 输入始终关联普通说明，并在自建 Responses WS 时追加可宣告的安全警告', () => {
  assert.match(source, /const otherHelperTextId = 'helper-text-channel-other-label'/);
  assert.match(source, /const otherWarningId = 'helper-text-channel-other-self-hosted-warning'/);
  assert.match(source, /inputProps=\{\{ 'aria-describedby': otherDescriptionIds \}\}/);
  assert.match(source, /<FormHelperText error id=\{otherHelperTextId\}>/);
  assert.match(source, /<FormHelperText id=\{otherHelperTextId\}>/);
  assert.match(source, /<FormHelperText id=\{otherWarningId\} role="alert"/);
  assert.doesNotMatch(source, /helper-tex-channel-other-label/);
});
