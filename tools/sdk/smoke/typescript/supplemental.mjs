import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { once } from 'node:events';
import { createHmac, randomUUID } from 'node:crypto';
import { setTimeout as delay } from 'node:timers/promises';
import { ManyForge, Session, SessionError, ManyForgeError, InvalidPayloadError, PaginationError, ContactToJSON, TicketToJSON, encodeJSON, decodeJSON } from '@manyforge/sdk';
import { FeedbackClient, TelemetryClient } from '@manyforge/sdk/public';
import { SignedFeedbackClient, MailingServerClient } from '@manyforge/sdk/server';
import { boundaryProbes } from './boundary-probe.mjs';

export async function supplemental({ fixture, contact, ticket, checks }) {
    await boundaryProbes(checks);
    let mode = 'contact';
    const exchanges = [];
    const secret = 'mfs_entire_issued_secret';
    const server = createServer(async (req, res) => {
        const parts = [];
        for await (const part of req) parts.push(part);
        const body = Buffer.concat(parts).toString();
        exchanges.push({ url: req.url, body, headers: req.headers });
        res.setHeader('X-Request-Id', 'sdk-transport-fixture');
        res.setHeader('Content-Type', 'application/json');
        if (mode === 'html') { res.writeHead(502, { 'Content-Type': 'text/html' }); res.end('<html>' + 'x'.repeat(70_000)); }
        else if (mode === 'stepup') { res.writeHead(401); res.end('{"code":"REAUTHENTICATION_FAILED","message":"secret server text"}'); }
        else if (mode === 'unknown-error') { res.writeHead(409); res.end('{"code":"FUTURE_SERVER_CODE","message":"a new error","details":{"enabled":false}}'); }
        else if (mode === 'invalid') res.end('{"id":');
        else if (mode === 'slow') { await delay(100); res.end(encodeJSON(ContactToJSON(contact), {})); }
        else if (mode === 'ticket') res.end(encodeJSON({ ...TicketToJSON(ticket), priority: 'future-priority', message_count: 9007199254740993n, future_property: false }, {}));
        else if (mode === 'pages') res.end(encodeJSON({ items: [ContactToJSON(contact)], next_cursor: 'same-cursor' }, {}));
        else if (mode === 'sign') {
            const mailing = req.headers['x-mailing-timestamp'];
            const value = req.headers['x-feedback-signature'];
            const timestamp = mailing ?? /t=(\d+),v1=/.exec(value ?? '')?.[1];
            const actual = mailing ? req.headers['x-mailing-signature'] : value?.split('v1=')[1];
            const target = mailing ? req.url.split('?')[0] : req.url;
            const expected = createHmac('sha256', secret).update(`${timestamp}.${req.method}.${target}.${body}`).digest('hex');
            res.writeHead(actual === expected ? 418 : 401);
            res.end(JSON.stringify({ code: actual === expected ? 'SIGNATURE_VALID' : 'SIGNATURE_INVALID', message: 'signing fixture' }));
        } else res.end(encodeJSON(ContactToJSON(contact), {}));
    });
    server.listen(0, '127.0.0.1');
    await once(server, 'listening');
    const baseUrl = `http://127.0.0.1:${server.address().port}`;
    try {
        for (const url of [`${baseUrl}/api/v1`, `${baseUrl}/prefix`, `${baseUrl}?x=1`, `${baseUrl}#x`, 'https://user:password@example.test']) assert.throws(() => new ManyForge({ baseUrl: url }), TypeError);
        assert.throws(() => new ManyForge({ baseUrl, accessToken: 'a', tokenProvider: () => 'b' }), TypeError);
        assert.throws(() => new FeedbackClient({ baseUrl, publishableKey: 'key', accessToken: 'must-not-leak' }), TypeError);
        const client = new ManyForge({ baseUrl: `${baseUrl}/`, accessToken: 'fixture-token' });
        await client.business('scope/?!%').contacts.create({ body: { primaryEmail: contact.primaryEmail, displayName: null, enabled: false, count: 0, values: [], large: 9007199254740993n } });
        const wire = exchanges.at(-1);
        assert.ok(wire.url.includes('scope%2F%3F%21%25'));
        assert.match(wire.body, /"large":9007199254740993/);
        const decoded = decodeJSON(wire.body, {});
        assert.equal(decoded.enabled, false); assert.equal(decoded.count, 0); assert.deepEqual(decoded.values, []); assert.equal(decoded.display_name, null);
        mode = 'ticket';
        const future = await client.business('scope').tickets.get({ tid: 'ticket' });
        assert.equal(future.messageCount, 9007199254740993n); assert.equal(future.priority, 'future-priority'); assert.equal(future.future_property, false);
        mode = 'unknown-error';
        await assert.rejects(client.account.get(), error => error instanceof ManyForgeError && error.code === 'FUTURE_SERVER_CODE' && error.requestId === 'sdk-transport-fixture');
        mode = 'html';
        await assert.rejects(client.account.get(), error => error instanceof ManyForgeError && error.status === 502 && error.rawBody.byteLength === 65_536 && error.truncated && !String(error).includes('<html>'));
        mode = 'invalid'; await assert.rejects(client.account.get(), InvalidPayloadError);
        mode = 'pages';
        await assert.rejects(async () => { for await (const item of client.business('scope').contacts.iter({ limit: 1 })) assert.equal(item.id, contact.id); }, PaginationError);
        checks.push('native-transport-url-presence-bigint-unknown-enums-errors-pagination');

        mode = 'stepup';
        let rotations = 0;
        const session = Session.fromTokenPair({ accessToken: 'access', refreshToken: 'refresh', expiresIn: 300 }, { onRotate: () => { rotations++; } });
        const managed = new ManyForge({ baseUrl, session });
        const before401 = exchanges.length;
        await assert.rejects(managed.account.get(), error => error instanceof ManyForgeError && error.code === 'REAUTHENTICATION_FAILED' && !String(error).includes('secret server text'));
        await assert.rejects(new FeedbackClient({ baseUrl, publishableKey: 'public-key' }).posts.list(), error => error instanceof ManyForgeError && error.status === 401);
        assert.equal(exchanges.length - before401, 2); assert.equal(rotations, 0);
        const publicRequest = exchanges.at(-1);
        assert.equal(publicRequest.headers.authorization, undefined); assert.equal(publicRequest.headers.cookie, undefined);
        mode = 'slow';
        const beforeTimeout = exchanges.length;
        await assert.rejects(client.business('scope').contacts.create({ body: { primaryEmail: contact.primaryEmail } }, { timeoutMs: 20 }), error => error.name === 'TimeoutError');
        await delay(120); assert.equal(exchanges.length, beforeTimeout + 1);
        const abort = new AbortController();
        const pending = client.business('scope').contacts.create({ body: { primaryEmail: contact.primaryEmail } }, { signal: abort.signal });
        await delay(20); abort.abort();
        await assert.rejects(pending, error => error.name === 'AbortError');
        await delay(120); assert.equal(exchanges.length, beforeTimeout + 2);
        let releaseToken;
        const deferredToken = new Promise(resolve => { releaseToken = resolve; });
        const waitingAbort = new AbortController();
        const waiting = new ManyForge({ baseUrl, tokenProvider: () => deferredToken }).account.get({}, { signal: waitingAbort.signal });
        waitingAbort.abort();
        await assert.rejects(waiting, error => error.name === 'AbortError');
        releaseToken('fixture-token');
        await delay(20);
        assert.equal(exchanges.length, beforeTimeout + 2);
        checks.push('no-business-or-public-401-replay-native-timeout-cancellation');

        let refreshCalls = 0;
        const persistenceSession = Session.fromTokenPair({ accessToken: 'old', refreshToken: 'old-refresh', expiresIn: 0.01 }, { onRotate: async () => { throw new Error('durable store failed'); } });
        const persistenceClient = new ManyForge({ baseUrl, session: persistenceSession, fetch: async () => { refreshCalls++; return new Response('{"access_token":"new-access","refresh_token":"new-refresh","expires_in":300}', { status: 200 }); } });
        await delay(15);
        await assert.rejects(persistenceClient.account.get(), SessionError);
        await assert.rejects(persistenceClient.account.get(), SessionError);
        assert.equal(refreshCalls, 1);
        checks.push('failed-durable-rotation-invalidates-without-old-token-retry');

        mode = 'sign';
        const signed = new SignedFeedbackClient({ baseUrl, publishableKey: 'key/?% !', signingSecret: secret });
        const params = { limit: 2, voterIdentity: 'voter +/?&=% café', author: 'author?&= +/' };
        await assert.rejects(signed.posts.list(params), error => error instanceof ManyForgeError && error.code === 'SIGNATURE_VALID');
        assert.ok(exchanges.at(-1).url.includes('voter_identity='));
        const tampered = new SignedFeedbackClient({ baseUrl, publishableKey: 'key/?% !', signingSecret: secret, fetch: (input, init) => { const url = new URL(input); url.search = new URLSearchParams([...url.searchParams].reverse()).toString(); return fetch(url, init); } });
        await assert.rejects(tampered.posts.list(params), error => error instanceof ManyForgeError && error.code === 'SIGNATURE_INVALID');
        const mailing = new MailingServerClient({ baseUrl, publishableKey: 'key/?% !', signingSecret: secret });
        await assert.rejects(mailing.mailing.events.create({ body: { email: 'sdk@example.test', name: 'sdk-event', idempotencyKey: randomUUID(), properties: { count: 9007199254740993n } } }), error => error instanceof ManyForgeError && error.code === 'SIGNATURE_VALID');
        checks.push('exact-encoded-query-signing-tampering-distinct-mailing-protocol');

        await assert.rejects(new FeedbackClient({ baseUrl: fixture.redirect_base_url, publishableKey: 'redirect-key' }).posts.list(), error => error instanceof ManyForgeError && error.status >= 300 && error.status < 400);
        checks.push('native-redirect-metadata-no-destination-forwarding');
    } finally { server.closeAllConnections(); await new Promise((resolve, reject) => server.close(error => error ? reject(error) : resolve())); }
}
