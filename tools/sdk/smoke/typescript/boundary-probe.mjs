import assert from 'node:assert/strict';
import { inspect } from 'node:util';
import { pathToFileURL } from 'node:url';
import { setTimeout as delay } from 'node:timers/promises';
import { ManyForge, Session } from '@manyforge/sdk';

const baseUrl = 'http://127.0.0.1:1';
const importParams = data => ({ lid: 'list', consentAttested: true, skipConfirmation: true, file: { filename: 'subscribers.csv', data, contentType: 'text/csv' } });

export async function credentialHeaderProbe() {
    const secret = 'boundary-secret-must-not-appear';
    const malformed = [`${secret}\r\ninjected`, `${secret}\r`, `${secret}\n`, `${secret}\0`, `${secret}\t`, `${secret}\x7f`, `${secret}\u0100`, ` ${secret}`, `${secret} `, `${secret} extra`];
    for (const mode of ['fixed', 'provider', 'session']) {
        for (const token of malformed) {
            let fetches = 0;
            let failure;
            try {
                const credentials = mode === 'fixed' ? { accessToken: token }
                    : mode === 'provider' ? { tokenProvider: async () => token }
                    : { session: Session.fromTokenPair({ accessToken: token, refreshToken: 'refresh', expiresIn: 300 }) };
                const client = new ManyForge({ baseUrl, ...credentials, fetch: async () => { fetches++; return new Response(null, { status: 204 }); } });
                await client.account.get();
            } catch (error) { failure = error; }
            assert.ok(failure instanceof TypeError, `${mode}: malformed access token must be rejected`);
            const diagnostics = [String(failure), inspect(failure, { showHidden: true, depth: null }), JSON.stringify(failure)].join('\n');
            assert.equal(diagnostics.includes(secret), false, `${mode}: credential leaked through default error diagnostics`);
            assert.equal(failure.cause, undefined, `${mode}: native validation cause must not be retained`);
            assert.equal(fetches, 0, `${mode}: malformed credentials must never reach fetch`);
        }
    }
    const token = 'valid._~+/-==';
    let authorization;
    await new ManyForge({ baseUrl, accessToken: token, fetch: async (_input, init) => { authorization = init.headers.get('Authorization'); return new Response(null, { status: 204 }); } }).account.get();
    assert.equal(authorization, `Bearer ${token}`);
}

export async function uploadCancellationProbe() {
    for (const mode of ['abort', 'timeout']) {
        let sourceController;
        let releasePull;
        let enteredPull;
        let pulls = 0;
        let cancellations = 0;
        let cancellationReason;
        let fetches = 0;
        const reading = new Promise(resolve => { enteredPull = resolve; });
        const stalled = new Promise(resolve => { releasePull = resolve; });
        const stream = new ReadableStream({
            start(controller) { sourceController = controller; },
            pull(controller) {
                pulls++;
                if (pulls === 1) { controller.enqueue(new TextEncoder().encode('email\n')); return; }
                enteredPull();
                return stalled;
            },
            cancel(reason) {
                cancellations++;
                cancellationReason = reason;
                // Neither a stalled nor a failing source cleanup may delay/rewrite cancellation.
                return mode === 'abort' ? new Promise(() => {}) : Promise.reject(new Error('source cleanup failed'));
            },
        }, { highWaterMark: 0 });
        const abort = new AbortController();
        const reason = new DOMException('Caller cancelled upload', 'AbortError');
        const client = new ManyForge({ baseUrl, accessToken: 'valid', fetch: async () => { fetches++; return new Response(null, { status: 204 }); } });
        const pending = client.business('business').mailing.subscribers.importCsv(importParams(stream), mode === 'abort' ? { signal: abort.signal } : { timeoutMs: 100 });
        const outcome = pending.then(() => ({ success: true }), error => ({ error }));
        try {
            await reading;
            assert.equal(stream.locked, true);
            if (mode === 'abort') abort.abort(reason);
            const { error } = await outcome;
            if (mode === 'abort') assert.equal(error, reason, 'AbortSignal reason identity must be preserved');
            else assert.equal(error?.name, 'TimeoutError');
            assert.equal(cancellations, 1, `${mode}: upload source must be cancelled exactly once`);
            assert.equal(cancellationReason, error, `${mode}: source must receive the request cancellation reason`);
            assert.equal(stream.locked, false, `${mode}: reader ownership must be released before rejection`);
            const stoppedPulls = pulls;
            releasePull();
            await delay(20);
            assert.equal(pulls, stoppedPulls, `${mode}: cancelled upload must not continue pulling`);
            assert.equal(fetches, 0, `${mode}: partial upload must never reach fetch`);
        } finally {
            releasePull();
            // Let the old package's abandoned Response drain so a failing probe terminates cleanly.
            try { sourceController.close(); } catch { /* Already cancelled by the corrected runtime. */ }
            await outcome;
        }
    }
}

export async function uploadPreservationProbe() {
    const csv = 'email,name\nsdk@example.test,SDK\n';
    const bytes = new TextEncoder().encode(csv);
    const streamed = new ReadableStream({
        start(controller) { controller.enqueue(bytes.subarray(0, 5)); controller.enqueue(bytes.subarray(5)); controller.close(); },
    });
    const payloads = [bytes, new Blob([bytes], { type: 'text/csv' }), new File([bytes], 'original.csv', { type: 'text/csv' }), streamed];
    for (const data of payloads) {
        let fetches = 0;
        const client = new ManyForge({ baseUrl, accessToken: 'valid', fetch: async (_input, init) => {
            fetches++;
            const file = init.body.get('file');
            assert.equal(file.name, 'subscribers.csv');
            assert.equal(await file.text(), csv);
            assert.equal(init.body.get('consent_attested'), 'true');
            assert.equal(init.body.get('skip_confirmation'), 'true');
            return new Response('{"errors":[],"imported":1,"skipped":0}', { status: 200 });
        } });
        const result = await client.business('business').mailing.subscribers.importCsv(importParams(data));
        assert.equal(result.imported, 1n);
        assert.equal(fetches, 1);
    }
    assert.equal(streamed.locked, false);
}

export async function boundaryProbes(checks) {
    await credentialHeaderProbe();
    checks.push('credential-header-validation-no-secret-diagnostics');
    await uploadCancellationProbe();
    await uploadPreservationProbe();
    checks.push('stream-upload-cancellation-ownership-no-partial-send');
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
    const probes = { credentials: credentialHeaderProbe, cancellation: uploadCancellationProbe, uploads: uploadPreservationProbe };
    const selected = process.argv[2];
    if (selected && !probes[selected]) throw new TypeError('Expected credentials, cancellation, or uploads');
    for (const [name, probe] of Object.entries(probes)) {
        if (selected && selected !== name) continue;
        await probe();
        console.log(`${name}: passed`);
    }
}
