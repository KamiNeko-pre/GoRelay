// Trusted read-only browser runtime. A model supplies declarative locators,
// never JavaScript. Public egress is pinned and budgeted by the Go proxy.
'use strict';
const fs = require('node:fs');

async function main() {
  const input = JSON.parse(fs.readFileSync(0, 'utf8'));
  const { chromium } = require(input.module_path || 'playwright');
  const output = { sources: [], requests: [], error: '' };
  let browser;
  try {
    browser = await chromium.launch({ headless: true, executablePath: input.executable_path,
      proxy: { server: input.proxy }, args: ['--disable-quic', '--proxy-bypass-list=<-loopback>'] });
    const context = await browser.newContext({ serviceWorkers: 'block', acceptDownloads: false });
    let count = 0;
    let policyError = '';
    await context.route('**/*', async route => {
      const request = route.request();
      let allowed = false;
      try {
        const u = new URL(request.url());
        allowed = ['http:', 'https:'].includes(u.protocol) && !u.username && !u.password &&
          ['GET', 'HEAD'].includes(request.method()) &&
          (!input.scope.length || input.scope.some(d => u.hostname === d || u.hostname.endsWith('.' + d)));
      } catch (_) {}
      if (!allowed || ++count > input.max_requests) {
        policyError = !allowed ? 'browser request outside read-only source scope' : 'browser HTTP request budget exceeded';
        await route.abort('blockedbyclient');
      } else { await route.continue(); }
    });
    await context.routeWebSocket('**/*', socket => socket.close());
    context.on('response', response => output.requests.push({ method: response.request().method(),
      requested_url: response.request().url(), url: response.url(), status: response.status(),
      fetched_at: new Date().toISOString() }));
    const page = await context.newPage();
    const deadline = input.timeout_ms > 0 ? Date.now() + input.timeout_ms : Infinity;
    const actionTimeout = Math.max(1,input.action_timeout_ms);
    page.setDefaultTimeout(Math.min(deadline-Date.now(), actionTimeout));
    let target = input.plan.url;
    const seen = new Set();
    while (target) {
      if (Date.now() >= deadline) throw new Error('browser request deadline reached');
      if (seen.has(target)) throw new Error('browser pagination revisited the same URL');
      seen.add(target);
      const response = await page.goto(target, { waitUntil: input.plan.wait_selector ? 'domcontentloaded' : 'networkidle', timeout: Math.max(1, Math.min(actionTimeout, deadline-Date.now())) });
      if (!response || response.status() < 200 || response.status() >= 300) throw new Error('browser document HTTP status ' + (response ? response.status() : 'unknown'));
      if (input.plan.wait_selector) await page.locator(input.plan.wait_selector).first().waitFor({state:'attached'});
      const body = await page.content();
      if (Buffer.byteLength(body) > input.max_bytes) throw new Error('rendered DOM exceeds the acquisition byte budget');
      const receipt = {method:'GET', requested_url:target, url:page.url(), status:response.status(), bytes:Buffer.byteLength(body), fetched_at:new Date().toISOString()};
      output.sources.push({body,receipt});
      target = '';
      if (input.plan.next_selector) {
        const next = page.locator(input.plan.next_selector).first();
        if (await next.count()) {
          const href = await next.getAttribute('href');
          if (href) target = new URL(href,page.url()).href;
          else throw new Error('next_selector has no href; declare a navigable next link or use an explicitly supported interaction plan');
        }
      }
      if (policyError) throw new Error(policyError);
    }
    await context.close();
  } catch (error) { output.error = error.message || String(error); }
  finally { if (browser) await browser.close(); }
  process.stdout.write(JSON.stringify(output));
}
main().catch(error => { process.stdout.write(JSON.stringify({sources:[],requests:[],error:error.message})); process.exitCode=1; });
