import { decodeJSON, type JsonValue } from './model-support.js';

/** HTTP failures retain diagnostic data only in explicit properties, never the message. */
export class ManyForgeError extends Error {
    readonly code?: string;
    readonly serverMessage?: string;
    readonly requestId?: string;
    readonly details?: JsonValue;
    constructor(readonly status: number, readonly headers: Headers, readonly rawBody: Uint8Array, readonly truncated: boolean) {
        super(`ManyForge HTTP request failed (${status})`);
        this.name = 'ManyForgeError';
        this.requestId = headers.get('x-request-id') ?? undefined;
        try {
            const data = decodeJSON<JsonValue>(new TextDecoder().decode(rawBody), {});
            this.details = data;
            if (data && typeof data === 'object' && !Array.isArray(data)) {
                if (typeof data.code === 'string') this.code = data.code;
                if (typeof data.message === 'string') this.serverMessage = data.message;
                else if (typeof data.error === 'string') this.serverMessage = data.error;
            }
        } catch { /* HTML, invalid and truncated JSON remain available as bytes. */ }
        for (const key of ['headers', 'rawBody', 'details', 'code', 'serverMessage', 'requestId']) {
            Object.defineProperty(this, key, { enumerable: false });
        }
    }
}

export class RedirectBlockedError extends Error {
    readonly status = undefined;
    readonly headers = undefined;
    constructor() { super('Browser blocked a redirect; HTTP metadata is unavailable'); this.name = 'RedirectBlockedError'; }
}

export class SessionError extends Error {
    constructor() { super('Session is unusable; authenticate again'); this.name = 'SessionError'; }
}
