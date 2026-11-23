function responseError(response, message) {
  return Object.assign(new Error(message), { status: response.status, httpStatus: response.status });
}

export async function requestJSON(path, { token = globalThis.sessionStorage?.getItem('gorelay-admin-token') || '', timeoutMs = 0, ...options } = {}) {
  const headers = new Headers(options.headers);
  headers.set('Accept', 'application/json');
  headers.set('Content-Type', 'application/json');
  if (token) headers.set('Authorization', 'Bearer ' + token);
  if (timeoutMs > 0 && !options.signal) options.signal = AbortSignal.timeout(timeoutMs);
  const response = await fetch('/api/v1' + path, { ...options, headers });
  let body;
  try { body = await response.json(); }
  catch { throw responseError(response, '服务返回了无法读取的响应（HTTP ' + response.status + '）'); }
  if (!response.ok) throw responseError(response, body?.message || body?.error || '请求失败（HTTP ' + response.status + '）');
  return body;
}
