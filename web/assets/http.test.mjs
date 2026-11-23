import assert from 'node:assert/strict';
import test from 'node:test';
import { requestJSON } from './http.mjs';
import { fetchTask } from './api.js';

test('shared transport preserves auth, raw/enveloped results and actionable errors', async () => {
  const originalFetch = globalThis.fetch;
  const originalStorage = globalThis.sessionStorage;
  globalThis.sessionStorage = { getItem: () => 'saved-fixture' };
  try {
    globalThis.fetch = async (url, options) => {
      assert.equal(url, '/api/v1/tasks/7');
      assert.equal(options.headers.get('Authorization'), 'Bearer saved-fixture');
      return { ok: true, status: 200, json: async () => ({ data: { id: 7 } }) };
    };
    assert.deepEqual(await requestJSON('/tasks/7'), { data: { id: 7 } });
    assert.deepEqual(await fetchTask(7), { id: 7 });
    globalThis.fetch = async (_url, options) => {
      assert.equal(options.headers.get('Authorization'), 'Bearer current-fixture');
      assert.equal(options.signal, undefined);
      return { ok: false, status: 404, json: async () => ({ error: '任务不存在' }) };
    };
    await assert.rejects(requestJSON('/tasks/missing', { token: 'current-fixture' }), error => error.status === 404 && error.httpStatus === 404 && error.message === '任务不存在');
    globalThis.fetch = async () => ({ ok: true, status: 200, json: async () => { throw new SyntaxError('invalid JSON'); } });
    await assert.rejects(requestJSON('/tasks/7'), /无法读取/);
  } finally {
    globalThis.fetch = originalFetch;
    if (originalStorage === undefined) delete globalThis.sessionStorage; else globalThis.sessionStorage = originalStorage;
  }
});
