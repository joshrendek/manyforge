import { convert, encodeJSON } from './model-support.js';
import { ManyForgeError, RedirectBlockedError } from './errors.js';
import { RootAuthApi } from './apis/RootAuthApi.js';
import type { Session } from './session.js';
import type { ByteStream, OperationRequest, RequestOptions, Transport, Upload } from './transport.js';

interface ConnectionOptions {
    readonly baseUrl: string;
    readonly timeoutMs?: number;
    /** Must honor explicit headers, credentials:omit, redirect:manual and no ambiguous replay. */
    readonly fetch?: typeof globalThis.fetch;
}
export type ClientOptions = ConnectionOptions & (
    | { readonly accessToken?: string; readonly tokenProvider?: never; readonly session?: never }
    | { readonly accessToken?: never; readonly tokenProvider: () => string | Promise<string>; readonly session?: never }
    | { readonly accessToken?: never; readonly tokenProvider?: never; readonly session: Session }
);
export interface PublicClientOptions {
    readonly baseUrl: string;
    readonly publishableKey: string;
    readonly timeoutMs?: number;
    readonly fetch?: typeof globalThis.fetch;
}
export type SignRequest = (method: string, target: string, body: string) => Readonly<Record<string, string>>;

async function cancellable<T>(pending: Promise<T>, signal: AbortSignal): Promise<T> {
    signal.throwIfAborted();
    let abort!: () => void;
    const cancellation = new Promise<never>((_, reject) => {
        abort = () => reject(signal.reason);
        signal.addEventListener('abort', abort, { once: true });
    });
    try { return await Promise.race([pending, cancellation]); }
    finally { signal.removeEventListener('abort', abort); }
}

function validateAccessToken(token: unknown): asserts token is string {
    // RFC 6750 bearer-token syntax also excludes native header-validation failures.
    if (typeof token !== 'string' || !/^[A-Za-z0-9._~+/-]+=*(?![\s\S])/.test(token)) {
        throw new TypeError('An authenticated request requires a valid nonempty bearer access token');
    }
}

async function uploadBlob(stream: ReadableStream<Uint8Array>, signal: AbortSignal): Promise<Blob> {
    signal.throwIfAborted();
    const reader = stream.getReader();
    // Do not await the source's cancellation hook: it may never settle.
    const abort = () => { void reader.cancel(signal.reason).catch(() => {}); };
    signal.addEventListener('abort', abort, { once: true });
    try {
        const chunks: Blob[] = [];
        while (true) {
            signal.throwIfAborted();
            const { done, value } = await cancellable(reader.read(), signal);
            signal.throwIfAborted();
            if (done) return new Blob(chunks);
            if (!(value instanceof Uint8Array)) throw new TypeError('Upload streams must contain bytes');
            // Snapshot each chunk before the source can reuse its backing buffer.
            chunks.push(new Blob([value as BlobPart]));
        }
    } catch (error) {
        if (!signal.aborted) void reader.cancel(error).catch(() => {});
        throw error;
    } finally {
        signal.removeEventListener('abort', abort);
        reader.releaseLock();
    }
}

export function instanceOrigin(value: string): string {
    let url: URL;
    try { url = new URL(value); }
    catch { throw new TypeError('Expected an absolute instance-root HTTP(S) URL'); }
    if (!['https:', 'http:'].includes(url.protocol) || url.username || url.password || url.search || url.hash || url.pathname !== '/' || /[?#]/.test(value)) throw new TypeError('baseUrl must be an absolute instance-root HTTP(S) URL');
    return url.origin;
}

export function publicTransport(options: PublicClientOptions, extra: { sourceOrigin?: string; sign?: SignRequest } = {}): FetchTransport {
    for (const key of ['accessToken', 'tokenProvider', 'session', 'signingSecret', 'sourceOrigin']) {
        if (key in options) throw new TypeError('Public client options cannot contain credentials or an origin override');
    }
    if (!options.publishableKey?.trim()) throw new TypeError('A nonempty publishableKey is required');
    return new FetchTransport(options, { publishableKey: options.publishableKey, ...extra });
}

export class FetchTransport implements Transport {
    readonly #origin: string;
    readonly #fetch: typeof globalThis.fetch;
    readonly #options: ClientOptions;
    readonly #public?: { publishableKey: string; sourceOrigin?: string; sign?: SignRequest };
    constructor(options: ClientOptions, publicOptions?: { publishableKey: string; sourceOrigin?: string; sign?: SignRequest }) {
        this.#origin = instanceOrigin(options.baseUrl);
        if ([options.accessToken, options.tokenProvider, options.session].filter(value => value !== undefined).length > 1) throw new TypeError('accessToken, tokenProvider and session are mutually exclusive');
        if (options.accessToken !== undefined) validateAccessToken(options.accessToken);
        if (options.timeoutMs !== undefined && (!Number.isFinite(options.timeoutMs) || options.timeoutMs <= 0)) throw new TypeError('timeoutMs must be positive');
        this.#options = { ...options };
        this.#fetch = options.fetch ?? globalThis.fetch.bind(globalThis);
        this.#public = publicOptions;
    }
    async request<T>(request: OperationRequest<T>, options: RequestOptions = {}): Promise<T> {
        const timeout = options.timeoutMs ?? this.#options.timeoutMs ?? 30_000;
        if (!Number.isFinite(timeout) || timeout <= 0) throw new TypeError('timeoutMs must be positive');
        const deadline = new AbortController();
        const timer = setTimeout(() => deadline.abort(new DOMException('Request timed out', 'TimeoutError')), timeout);
        const signal = options.signal ? AbortSignal.any([options.signal, deadline.signal]) : deadline.signal;
        let streaming = false;
        try {
            signal.throwIfAborted();
            if (this.#public && request.authenticated) throw new TypeError('Public clients cannot make authenticated requests');
            const path = request.path.replace(/\{([^}]+)\}/g, (_, name: string) => {
                const value = request.pathParams[name];
                if (value === undefined || value === '' || value === '.' || value === '..') throw new TypeError('Invalid path parameter');
                return encodeURIComponent(value).replace(/[!'()*]/g, char => `%${char.charCodeAt(0).toString(16).toUpperCase()}`);
            });
            if (!path.startsWith('/') || path.startsWith('//') || /[?#]/.test(path)) throw new TypeError('Generated request path must be relative to the instance root');
            const query = new URLSearchParams();
            for (const param of request.query) {
                if (param.value === undefined || param.value === null) continue;
                if (Array.isArray(param.value)) {
                    if (param.explode) for (const value of param.value) query.append(param.name, String(value));
                    else query.append(param.name, param.value.map(String).join(param.style === 'spaceDelimited' ? ' ' : param.style === 'pipeDelimited' ? '|' : ','));
                } else if (typeof param.value === 'object') {
                    const entries = Object.entries(param.value);
                    if (param.style === 'deepObject') for (const [key, value] of entries) query.append(`${param.name}[${key}]`, String(value));
                    else if (param.explode) for (const [key, value] of entries) query.append(key, String(value));
                    else query.append(param.name, entries.flatMap(([key, value]) => [key, String(value)]).join(','));
                } else query.append(param.name, String(param.value));
            }
            const target = path + (query.size ? `?${query}` : '');
            const headers = new Headers();
            for (const [key, value] of Object.entries(request.headers)) {
                if (value !== undefined && !/^(authorization|cookie|origin|x-.*signature|x-mailing-timestamp)$/i.test(key)) headers.set(key, String(value));
            }
            if (request.authenticated) {
                const pending = this.#options.session
                    ? this.#options.session.accessToken(this.#origin, refreshToken => new RootAuthApi(this).refresh({ body: { refreshToken } }))
                    : Promise.resolve(this.#options.tokenProvider ? this.#options.tokenProvider() : this.#options.accessToken);
                const token = await cancellable(pending, signal);
                signal.throwIfAborted();
                validateAccessToken(token);
                headers.set('Authorization', `Bearer ${token}`);
            }
            if (this.#public?.sourceOrigin) headers.set('Origin', this.#public.sourceOrigin);
            let body: BodyInit | undefined;
            let signedBody = '';
            if (request.body !== undefined) {
                const encoded = convert(request.bodySchema ?? {}, request.body, 'encode');
                if (request.bodyKey) {
                    if (!this.#public || !encoded || typeof encoded !== 'object' || Array.isArray(encoded)) throw new TypeError('Public key injection requires a public JSON object');
                    (encoded as Record<string, unknown>)[request.bodyKey] = this.#public.publishableKey;
                }
                signedBody = encodeJSON(encoded, {});
                body = signedBody;
                headers.set('Content-Type', request.contentType ?? 'application/json');
            } else if (request.form) {
                const form = new FormData();
                for (const [key, value] of Object.entries(request.form)) {
                    if (value === undefined || value === null) continue;
                    if (typeof value === 'object' && 'filename' in value && 'data' in value) {
                        const upload = value as Upload;
                        const blob = upload.data instanceof Blob ? upload.data : upload.data instanceof ReadableStream
                            ? await uploadBlob(upload.data, signal) : new Blob([upload.data as BlobPart], { type: upload.contentType });
                        form.append(key, blob, upload.filename);
                    } else form.append(key, String(value));
                }
                body = form;
            }
            if (this.#public?.sign) for (const [key, value] of Object.entries(this.#public.sign(request.method, target, signedBody))) headers.set(key, value);
            signal.throwIfAborted();
            const response = await this.#fetch(this.#origin + target, { method: request.method, headers, body, signal, redirect: 'manual', credentials: 'omit' });
            if (response.type === 'opaqueredirect') throw new RedirectBlockedError();
            if (!response.ok) {
                const reader = response.body?.getReader();
                const chunks: Uint8Array[] = [];
                let length = 0;
                let truncated = false;
                try {
                    while (reader) {
                        const { done, value } = await reader.read();
                        if (done) break;
                        const remaining = 65_536 - length;
                        chunks.push(value.slice(0, remaining));
                        length += Math.min(value.byteLength, remaining);
                        if (value.byteLength > remaining) { truncated = true; break; }
                    }
                } finally { await reader?.cancel(); }
                const raw = new Uint8Array(length);
                let offset = 0;
                for (const chunk of chunks) { raw.set(chunk, offset); offset += chunk.byteLength; }
                throw new ManyForgeError(response.status, new Headers(response.headers), raw, truncated);
            }
            if (request.response === 'stream') {
                const reader = response.body?.getReader();
                const stream = new ReadableStream<Uint8Array>({
                    async pull(controller) {
                        try {
                            const next = await reader?.read();
                            if (!next || next.done) { clearTimeout(timer); controller.close(); }
                            else controller.enqueue(next.value);
                        } catch (error) { clearTimeout(timer); controller.error(error); }
                    },
                    async cancel(reason) { clearTimeout(timer); await reader?.cancel(reason); },
                });
                streaming = true;
                return { stream, async close() { clearTimeout(timer); await reader?.cancel(); } } as ByteStream as T;
            }
            if (request.response === 'empty') { await response.body?.cancel(); return undefined as T; }
            const text = await response.text();
            return text.length ? request.decode(text) : undefined as T;
        } finally { if (!streaming) clearTimeout(timer); }
    }
}
