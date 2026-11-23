// Trusted read-only browser runtime. A model supplies declarative locators,
// never JavaScript. Public egress is pinned by the Go proxy.
// Supports both one-shot stdin/stdout mode and persistent NDJSON `--worker` mode.
'use strict';
const fs = require('node:fs');
const readline = require('node:readline');

const BLOCKED_RESOURCE_TYPES = new Set(['image', 'media', 'font', 'websocket', 'eventsource']);

function sanitizeRequestHeaders(hdrs) {
  const out = {};
  let sentCookies = false;
  for (const [k, v] of Object.entries(hdrs || {})) {
    const lk = k.toLowerCase();
    if (lk === 'cookie') { if (v) sentCookies = true; continue; }
    if (lk === 'authorization') { out[lk] = '<redacted>'; continue; }
    if (lk.startsWith(':') || lk.startsWith('sec-') ||
        ['user-agent', 'accept-language', 'accept-encoding', 'connection', 'host', 'priority', 'content-length', 'origin', 'referer'].includes(lk)) {
      continue;
    }
    if (v === '' || v === 'undefined' || v === 'null') continue;
    out[lk] = ((lk.includes('token') || lk.includes('csrf')) && String(v).length > 20) ? '<session-token>' : String(v).slice(0, 200);
  }
  return { headers: Object.keys(out).length ? out : undefined, sentCookies };
}

function sleep(ms) {
  return new Promise(resolve => setTimeout(resolve, Math.max(0, ms)));
}

async function waitForPageReady(page, state, remainingTimeout) {
  const loadWait = Math.min(3000, remainingTimeout());
  if (loadWait > 0) {
    await page.waitForLoadState('load', { timeout: loadWait }).catch(() => {});
  }
  const settleCap = Date.now() + Math.min(4500, remainingTimeout());
  const startWait = Date.now();
  while (Date.now() < settleCap) {
    const now = Date.now();
    if (state.inflightAPI === 0) {
      if (state.sawAPI && now - state.lastAPIDoneAt >= 600) break;
      if (!state.sawAPI && now - startWait >= 1200) break;
    }
    await sleep(100);
  }
}

async function renderOne(browser, input, useContextProxy) {
  const scope = Array.isArray(input.scope) ? input.scope : [];
  const output = { sources: [], requests: [], network_apis: [], error: '' };
  const pendingAPIs = [];
  let context;
  try {
    const ctxOpts = { serviceWorkers: 'block', acceptDownloads: false };
    if (useContextProxy && input.proxy) ctxOpts.proxy = { server: input.proxy };
    context = await browser.newContext(ctxOpts);
    await context.route('**/*', async route => {
      const request = route.request();
      const rt = request.resourceType();
      if (BLOCKED_RESOURCE_TYPES.has(rt)) {
        await route.abort('blockedbyclient');
        return;
      }
      let allowed = false;
      try {
        const u = new URL(request.url());
        allowed = ['http:', 'https:'].includes(u.protocol) && !u.username && !u.password &&
          (!request.isNavigationRequest() || !scope.length || scope.some(d => u.hostname.replace(/\.$/, '') === d || u.hostname.replace(/\.$/, '').endsWith('.' + d)));
      } catch (_) {}
      if (!allowed) { await route.abort('blockedbyclient'); }
      else { await route.continue(); }
    });
    const apiState = { inflightAPI: 0, sawAPI: false, lastAPIDoneAt: Date.now() };
    const trackedReqs = new WeakSet();
    context.on('request', req => {
      const rt = req.resourceType();
      if (rt === 'xhr' || rt === 'fetch') {
        trackedReqs.add(req);
        apiState.inflightAPI++;
        apiState.sawAPI = true;
      }
    });
    const markDone = req => {
      if (trackedReqs.has(req)) {
        trackedReqs.delete(req);
        apiState.inflightAPI = Math.max(0, apiState.inflightAPI - 1);
        apiState.lastAPIDoneAt = Date.now();
      }
    };
    context.on('requestfinished', markDone);
    context.on('requestfailed', markDone);
    context.on('response', response => {
      const req = response.request();
      const rt = req.resourceType();
      const ct = (response.headers()['content-type'] || '').toLowerCase();
      const isAPI = rt === 'xhr' || rt === 'fetch' || ct.includes('application/json') || ct.includes('+json');
      if (rt !== 'document' && !isAPI) return;
      output.requests.push({ method: req.method(), requested_url: req.url(), url: response.url(), status: response.status(), fetched_at: new Date().toISOString() });
      if (!isAPI) return;
      pendingAPIs.push((async () => {
        try {
          const [reqHdrs, respHdrs] = await Promise.all([req.allHeaders().catch(() => ({})), response.allHeaders().catch(() => ({}))]);
          const cleaned = sanitizeRequestHeaders(reqHdrs);
          const entry = {
            method: req.method(),
            url: response.url(),
            status: response.status(),
            content_type: ct.split(';')[0].trim(),
            request_headers: cleaned.headers,
            sent_cookies: cleaned.sentCookies || undefined,
            sets_cookie: Boolean(respHdrs['set-cookie']) || undefined,
          };
          const postData = req.postData();
          if (postData) entry.request_body = postData.slice(0, 2048);
          if (ct.includes('json') && response.status() >= 200 && response.status() < 400) {
            const text = await response.text().catch(() => '');
            if (text && text.length <= 131072) entry.response_body = text;
          }
          output.network_apis.push(entry);
        } catch (_) {}
      })());
    });
    const page = await context.newPage();
    const deadline = input.timeout_ms > 0 ? Date.now() + input.timeout_ms : Infinity;
    const actionTimeout = input.action_timeout_ms > 0 ? input.action_timeout_ms : Infinity;
    const remainingTimeout = () => { const ms = Math.min(actionTimeout, deadline - Date.now()); return Number.isFinite(ms) ? Math.max(1, ms) : 0; };
    const defaultTimeout = Math.min(deadline - Date.now(), actionTimeout);
    if (Number.isFinite(defaultTimeout) && defaultTimeout > 0) page.setDefaultTimeout(defaultTimeout);
    let target = input.plan.url;
    const seen = new Set();
    while (target) {
      if (Date.now() >= deadline) throw new Error('browser request deadline reached');
      if (seen.has(target)) throw new Error('browser pagination revisited the same URL');
      seen.add(target);
      apiState.inflightAPI = 0;
      apiState.sawAPI = false;
      apiState.lastAPIDoneAt = Date.now();
      const response = await page.goto(target, { waitUntil: 'domcontentloaded', timeout: remainingTimeout() });
      if (!response || response.status() < 200 || response.status() >= 300) throw new Error('browser document HTTP status ' + (response ? response.status() : 'unknown'));
      const ready = input.plan.empty_state
        ? [input.plan.wait_selector || input.plan.rows_selector, input.plan.empty_state.selector].filter(Boolean).join(', ')
        : input.plan.wait_selector;
      if (ready) {
        await page.locator(ready).first().waitFor({ state: 'attached', timeout: remainingTimeout() });
      } else {
        await waitForPageReady(page, apiState, remainingTimeout);
      }
      const body = await page.content();
      const receipt = { method: 'GET', requested_url: target, url: page.url(), status: response.status(), bytes: Buffer.byteLength(body), fetched_at: new Date().toISOString() };
      output.sources.push({ body, receipt });
      target = '';
      if (input.plan.next_selector) {
        const next = page.locator(input.plan.next_selector).first();
        if (await next.count()) {
          const href = await next.getAttribute('href');
          if (href) target = new URL(href, page.url()).href;
          else throw new Error('next_selector has no href; declare a navigable next link or use an explicitly supported interaction plan');
        }
      }
    }
    await Promise.allSettled(pendingAPIs);
  } catch (error) {
    output.error = error.message || String(error);
  } finally {
    if (context) await context.close().catch(() => {});
  }
  return output;
}

async function runWorker() {
  let browser = null;
  let browserKey = '';
  const rl = readline.createInterface({ input: process.stdin, crlfDelay: Infinity });
  const ensureBrowser = async input => {
    const useProxy = Boolean(input.proxy);
    const key = `${input.module_path || 'playwright'}|${input.executable_path || ''}|${useProxy}`;
    if (browser && browser.isConnected() && browserKey === key) return browser;
    if (browser) await browser.close().catch(() => {});
    const { chromium } = require(input.module_path || 'playwright');
    const launchOpts = {
      headless: true,
      executablePath: input.executable_path,
      args: ['--disable-quic', '--proxy-bypass-list=<-loopback>'],
    };
    if (useProxy) launchOpts.proxy = { server: 'http://per-context' };
    browser = await chromium.launch(launchOpts);
    browserKey = key;
    return browser;
  };
  for await (const line of rl) {
    const trimmed = line.trim();
    if (!trimmed) continue;
    let out;
    try {
      const input = JSON.parse(trimmed);
      const b = await ensureBrowser(input);
      out = await renderOne(b, input, true);
    } catch (error) {
      out = { sources: [], requests: [], network_apis: [], error: error.message || String(error) };
    }
    process.stdout.write(JSON.stringify(out) + '\n');
  }
  if (browser) await browser.close().catch(() => {});
}

async function runOneShot() {
  const input = JSON.parse(fs.readFileSync(0, 'utf8'));
  const { chromium } = require(input.module_path || 'playwright');
  let browser;
  let output;
  try {
    const launchOpts = { headless: true, executablePath: input.executable_path, args: ['--disable-quic', '--proxy-bypass-list=<-loopback>'] };
    if (input.proxy) launchOpts.proxy = { server: input.proxy };
    browser = await chromium.launch(launchOpts);
    output = await renderOne(browser, input, false);
  } catch (error) {
    output = { sources: [], requests: [], network_apis: [], error: error.message || String(error) };
  } finally {
    if (browser) await browser.close().catch(() => {});
  }
  process.stdout.write(JSON.stringify(output));
}

if (process.argv.includes('--worker')) {
  runWorker().catch(error => {
    process.stdout.write(JSON.stringify({ sources: [], requests: [], network_apis: [], error: error.message || String(error) }) + '\n');
    process.exitCode = 1;
  });
} else {
  runOneShot().catch(error => {
    process.stdout.write(JSON.stringify({ sources: [], requests: [], network_apis: [], error: error.message || String(error) }));
    process.exitCode = 1;
  });
}
