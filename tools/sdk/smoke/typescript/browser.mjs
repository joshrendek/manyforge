import assert from 'node:assert/strict';
import { mkdir, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { build } from 'esbuild';
import { chromium } from 'playwright';
import { setTimeout as delay } from 'node:timers/promises';

export async function browserSmoke({ fixture, key, crash, required, analytics, event, checks, uid }) {
    const browserDir = join(fixture.consumer_dir, 'browser');
    await mkdir(browserDir, { recursive: true });
    await build({ stdin: { contents: `
        import { FeedbackClient, TelemetryClient, AnalyticsClient, ManyForgeError, RedirectBlockedError } from '@manyforge/sdk/public';
        import * as publicSDK from '@manyforge/sdk/public';
        window.sdk = { FeedbackClient, TelemetryClient, AnalyticsClient, ManyForgeError, RedirectBlockedError };
        window.publicExports = Object.keys(publicSDK);
    `, resolveDir: fixture.consumer_dir, sourcefile: 'installed-browser-consumer.js' }, outfile: join(browserDir, 'sdk.js'), bundle: true, platform: 'browser', format: 'esm', metafile: true }).then(result => {
        assert.ok(!Object.keys(result.metafile.inputs).some(path => /server-clients|node:crypto/.test(path)));
    });
    await assert.rejects(build({ stdin: { contents: "import {SignedFeedbackClient} from '@manyforge/sdk/server'; console.log(SignedFeedbackClient)", resolveDir: fixture.consumer_dir }, bundle: true, platform: 'browser', write: false, logLevel: 'silent' }));
    await writeFile(join(browserDir, 'index.html'), '<!doctype html><html><body><h1>Installed ManyForge browser consumer</h1><pre id="evidence"></pre><script type="module" src="/browser/sdk.js"></script></body></html>');
    const browser = await chromium.launch({ headless: true });
    const context = await browser.newContext();
    const api = new URL(fixture.base_url);
    await context.addCookies([{ name: 'sdk_must_not_send', value: 'fixture-cookie', domain: api.hostname, path: '/' }]);
    const allowed = await context.newPage();
    const denied = await context.newPage();
    const protocol = await context.newCDPSession(allowed);
    await protocol.send('Network.enable');
    let feedbackPreflights = 0;
    protocol.on('Network.requestWillBeSent', ({ request }) => {
        if (request.method === 'OPTIONS' && request.url === `${fixture.base_url}/api/v1/feedback/public/${key.publishableKey}/posts`) feedbackPreflights++;
    });
    const network = [];
    const consoleLines = [];
    for (const page of [allowed, denied]) {
        // CORS-blocked responses may omit Playwright's response event. Chromium's
        // extra-info event retains the actual status/headers without page access.
        const session = page === allowed ? protocol : await context.newCDPSession(page);
        if (page !== allowed) await session.send('Network.enable');
        const requests = new Map();
        const pending = new Map();
        const recordExtra = info => {
            const request = requests.get(info.requestId);
            if (!request) { pending.set(info.requestId, info); return; }
            if (![fixture.base_url, fixture.telemetry_peer_base_url].some(base => request.url.startsWith(base))) return;
            const headers = Object.fromEntries(Object.entries(info.headers).map(([name, value]) => [name.toLowerCase(), String(value)]));
            network.push({ method: request.method, status: info.statusCode, headers, url: new URL(request.url).pathname.replace(/mfk_[^/]+/g, '[publishable-key]'), source: 'chromium-extra-info' });
        };
        session.on('Network.requestWillBeSent', ({ requestId, request }) => {
            requests.set(requestId, request);
            const info = pending.get(requestId);
            if (info) { pending.delete(requestId); recordExtra(info); }
        });
        session.on('Network.responseReceivedExtraInfo', recordExtra);
        page.on('console', message => consoleLines.push(message.text()));
        page.on('response', async response => {
            if (!response.url().startsWith(fixture.base_url)) return;
            network.push({ method: response.request().method(), status: response.status(), headers: await response.allHeaders(), url: new URL(response.url()).pathname.replace(/mfk_[^/]+/g, '[publishable-key]') });
        });
    }
    const deniedTitles = [`Denied JSON ${uid}`, `Denied simple ${uid}`, `Denied opaque ${uid}`, `Denied Authorization ${uid}`, `Denied signature ${uid}`];
    try {
        await Promise.all([allowed.goto(`${fixture.allowed_origin}/browser/index.html`), denied.goto(`${fixture.denied_origin}/browser/index.html`)]);
        await Promise.all([allowed.waitForFunction(() => Boolean(window.sdk)), denied.waitForFunction(() => Boolean(window.sdk))]);
        assert.equal(await allowed.evaluate(() => window.publicExports.includes('SignedFeedbackClient') || window.publicExports.includes('MailingServerClient')), false);
        const credentials = [];
        const observeCredentials = request => { if (request.url().startsWith(fixture.base_url) && request.method() !== 'OPTIONS') credentials.push(request.allHeaders()); };
        allowed.on('request', observeCredentials);
        const normal = await allowed.evaluate(async ({ baseUrl, publishableKey, telemetryKey, analyticsKey, event, uid }) => {
            const feedback = new window.sdk.FeedbackClient({ baseUrl, publishableKey });
            const posts = await feedback.posts.list();
            const post = await feedback.posts.create({ body: { title: `Browser allowed ${uid}`, authorIdentity: 'browser@example.test' } });
            const first = await feedback.posts.vote({ postID: post.id, body: { voterIdentity: `browser-${uid}` } });
            const duplicate = await feedback.posts.vote({ postID: post.id, body: { voterIdentity: `browser-${uid}` } });
            const ingested = await new window.sdk.TelemetryClient({ baseUrl, publishableKey: telemetryKey }).ingest({ body: { crash: [event] } });
            const collected = await new window.sdk.AnalyticsClient({ baseUrl, publishableKey: analyticsKey }).collect({ body: { n: `sdk_browser_${uid}`, p: '/browser-sdk' } });
            if (collected !== undefined) throw new Error('Analytics must not claim acceptance');
            const response = await fetch(`${baseUrl}/api/v1/feedback/public/${publishableKey}/posts`, { credentials: 'omit' });
            return { id: post.id, identityVerified: post.identityVerified, voted: first.voted, duplicate: duplicate.voted, accepted: ingested.accepted, requestId: response.headers.get('x-request-id'), items: posts.items.length };
        }, { baseUrl: fixture.base_url, publishableKey: key.publishableKey, telemetryKey: crash.publishableKey, analyticsKey: analytics.publishableKey, event, uid });
        allowed.off('request', observeCredentials);
        assert.equal(normal.identityVerified, false); assert.equal(normal.voted, true); assert.equal(normal.duplicate, false); assert.equal(normal.accepted, 1); assert.ok(normal.requestId);
        for (const headers of await Promise.all(credentials)) { assert.equal(headers.authorization, undefined); assert.equal(headers.cookie, undefined); }
        checks.push('chromium-installed-public-list-post-vote-telemetry-credential-isolation');
        checks.push('chromium-analytics-browser-origin-existing-collector-cors');

        const deniedOutcome = await denied.evaluate(async ({ baseUrl, publishableKey, titles }) => {
            const results = [];
            try { await new window.sdk.FeedbackClient({ baseUrl, publishableKey }).posts.create({ body: { title: titles[0] } }); results.push('unexpected-success'); } catch (error) { results.push(error.name); }
            try { await fetch(`${baseUrl}/api/v1/feedback/public/${publishableKey}/posts`, { method: 'POST', headers: { 'Content-Type': 'text/plain' }, body: JSON.stringify({ title: titles[1] }), credentials: 'omit' }); results.push('unexpected-success'); } catch (error) { results.push(error.name); }
            return results;
        }, { baseUrl: fixture.base_url, publishableKey: key.publishableKey, titles: deniedTitles });
        assert.deepEqual(deniedOutcome, ['TypeError', 'TypeError']);
        const opaque = await allowed.evaluate(({ baseUrl, publishableKey, title }) => new Promise((resolve, reject) => {
            const frame = document.createElement('iframe'); frame.sandbox = 'allow-scripts';
            const timer = setTimeout(() => reject(new Error('Opaque frame did not respond')), 10_000);
            window.addEventListener('message', function receive(message) {
                if (message.source !== frame.contentWindow) return;
                clearTimeout(timer); window.removeEventListener('message', receive); frame.remove(); resolve(message.data);
            });
            frame.srcdoc = `<script>fetch(${JSON.stringify(`${baseUrl}/api/v1/feedback/public/${publishableKey}/posts`)},{method:'POST',headers:{'Content-Type':'text/plain'},credentials:'omit',body:${JSON.stringify(JSON.stringify({ title }))}}).then(()=>parent.postMessage('unexpected-success','*'),e=>parent.postMessage(e.name,'*'))<\/script>`;
            document.body.append(frame);
        }), { baseUrl: fixture.base_url, publishableKey: key.publishableKey, title: deniedTitles[2] });
        assert.equal(opaque, 'TypeError');
        const forbidden = await allowed.evaluate(async ({ baseUrl, publishableKey, titles }) => {
            const results = [];
            for (const [header, title] of [['Authorization', titles[3]], ['X-Feedback-Signature', titles[4]]]) {
                try { await fetch(`${baseUrl}/api/v1/feedback/public/${publishableKey}/posts`, { method: 'POST', headers: { 'Content-Type': 'application/json', [header]: 'fixture-forbidden' }, body: JSON.stringify({ title }), credentials: 'omit' }); results.push('unexpected-success'); } catch (error) { results.push(error.name); }
            }
            try { await fetch(`${baseUrl}/api/v1/me`, { headers: { Authorization: 'Bearer fixture' }, credentials: 'omit' }); results.push('unexpected-success'); } catch (error) { results.push(error.name); }
            return results;
        }, { baseUrl: fixture.base_url, publishableKey: key.publishableKey, titles: deniedTitles });
        assert.deepEqual(forbidden, ['TypeError', 'TypeError', 'TypeError']);
        checks.push('chromium-denied-json-simple-opaque-and-browser-controlled-forbidden-preflights');

        await delay(2500);
        const rate = await allowed.evaluate(async ({ baseUrl, peerUrl, telemetryKey, requiredKey, event, burst }) => {
            const telemetry = new window.sdk.TelemetryClient({ baseUrl, publishableKey: telemetryKey });
            const peer = new window.sdk.TelemetryClient({ baseUrl: peerUrl, publishableKey: telemetryKey });
            const results = await Promise.all(Array.from({ length: burst - 5 }, async () => { await telemetry.ingest({ body: { crash: [event] } }); return true; }));
            const peerResults = await Promise.all(Array.from({ length: burst - 5 }, async () => {
                try { await peer.ingest({ body: { crash: [event] } }); return { status: 202 }; }
                catch (error) { return { status: error.status, code: error.code, message: error.serverMessage, requestId: error.requestId }; }
            }));
            let otherKey;
            try { await new window.sdk.TelemetryClient({ baseUrl: peerUrl, publishableKey: requiredKey }).ingest({ body: { crash: [event] } }); otherKey = 202; } catch (error) { otherKey = error.status; }
            return { original: results.length, peerResults, otherKey };
        }, { baseUrl: fixture.base_url, peerUrl: fixture.telemetry_peer_base_url, telemetryKey: crash.publishableKey, requiredKey: required.publishableKey, event, burst: fixture.ingest_rate_burst });
        assert.equal(rate.original, fixture.ingest_rate_burst - 5);
        assert.ok(rate.peerResults.some(result => result.status === 429 && result.message === 'rate_limited' && result.requestId));
        assert.equal(rate.otherKey, 401);
        checks.push('chromium-independent-telemetry-key-rate-limit-readable');

        await delay(2500);
        // A completed POST primes the browser's actual CORS cache, not a mocked OPTIONS call.
        await allowed.evaluate(async ({ baseUrl, publishableKey }) => {
            await new window.sdk.FeedbackClient({ baseUrl, publishableKey }).posts.create({ body: { title: 'Cached preflight rate probe', idempotencyKey: crypto.randomUUID() } });
        }, { baseUrl: fixture.base_url, publishableKey: key.publishableKey });
        const preflightsBeforeFlood = feedbackPreflights;
        assert.ok(preflightsBeforeFlood > 0);
        const outer = await allowed.evaluate(async ({ baseUrl, publishableKey, burst }) => {
            const feedback = new window.sdk.FeedbackClient({ baseUrl, publishableKey });
            return Promise.all(Array.from({ length: burst * 4 }, async () => {
                try { await feedback.posts.create({ body: { title: 'Cached preflight rate probe', idempotencyKey: 'shared-browser-rate-probe' } }); return { status: 200 }; }
                catch (error) { return { status: error.status, code: error.code, requestId: error.requestId, retryAfter: error.headers?.get('retry-after') }; }
            }));
        }, { baseUrl: fixture.base_url, publishableKey: key.publishableKey, burst: fixture.ingest_rate_burst });
        assert.ok(outer.some(result => result.status === 429 && result.code === 'RATE_LIMITED' && result.requestId && result.retryAfter));
        assert.equal(feedbackPreflights, preflightsBeforeFlood);
        checks.push('chromium-outer-ip-429-readable-after-cached-json-preflight');
        await delay(2500);

        const disabled = await allowed.evaluate(async ({ baseUrl, publishableKey }) => {
            try { await new window.sdk.FeedbackClient({ baseUrl, publishableKey }).posts.list(); return 'unexpected-success'; } catch (error) { return error.name; }
        }, { baseUrl: fixture.disabled_cors_base_url, publishableKey: key.publishableKey });
        assert.equal(disabled, 'TypeError');
        const sameOriginPage = await context.newPage();
        await sameOriginPage.goto(`${fixture.disabled_cors_base_url}/readyz`);
        assert.equal(await sameOriginPage.evaluate(async ({ publishableKey }) => (await fetch(`/api/v1/feedback/public/${publishableKey}/posts`)).status, { publishableKey: key.publishableKey }), 200);
        await sameOriginPage.close();
        const redirected = await allowed.evaluate(async ({ baseUrl }) => {
            try { await new window.sdk.FeedbackClient({ baseUrl, publishableKey: 'redirect-key' }).posts.list(); return { unexpected: true }; }
            catch (error) { return { blocked: error instanceof window.sdk.RedirectBlockedError, statusUnavailable: error.status === undefined, headersUnavailable: error.headers === undefined }; }
        }, { baseUrl: fixture.redirect_base_url });
        assert.deepEqual(redirected, { blocked: true, statusUnavailable: true, headersUnavailable: true });
        checks.push('chromium-disabled-policy-same-origin-preservation-opaque-redirect');
        await delay(100);
        const corsResponses = network.filter(item => item.headers['access-control-allow-origin'] === fixture.allowed_origin);
        assert.ok(corsResponses.some(item => item.status === 429));
        assert.ok(corsResponses.some(item => item.status >= 200 && item.status < 300));
        for (const response of corsResponses) { assert.match(response.headers.vary, /origin/i); assert.equal(response.headers['access-control-allow-credentials'], undefined); }
        assert.ok(network.some(item => item.status === 403 && !item.headers['access-control-allow-origin']));
        await allowed.evaluate(checks => { document.querySelector('#evidence').textContent = checks.join('\n'); }, checks);
        await allowed.screenshot({ path: join(browserDir, 'allowed-proof.png'), fullPage: true });
        await denied.evaluate(outcome => { document.querySelector('#evidence').textContent = JSON.stringify(outcome); }, deniedOutcome);
        await denied.screenshot({ path: join(browserDir, 'denied-proof.png'), fullPage: true });
        await writeFile(join(browserDir, 'network-proof.json'), JSON.stringify({ normal, deniedOutcome, opaque, forbidden, rate, outer, redirected, preflightsBeforeFlood, network, consoleLines }, null, 2));
        return { denied_feedback_titles: deniedTitles, browser_assertions: checks.filter(check => check.startsWith('chromium-')) };
    } finally { await context.close(); await browser.close(); }
}
