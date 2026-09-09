import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';

test('API response interceptor preserves rejected HTTP responses for conflict recovery', async () => {
  const source = await readFile(new URL('./api.js', import.meta.url), 'utf8');
  assert.match(source, /showError\(error\);\s*return Promise\.reject\(error\);/);
});
