import type { Schema } from './model-support.js';

export interface RequestOptions {
    readonly timeoutMs?: number;
    readonly signal?: AbortSignal;
}

export interface Upload {
    readonly filename: string;
    readonly data: Uint8Array | Blob | ReadableStream<Uint8Array>;
    readonly contentType?: string;
}

export interface ByteStream {
    readonly stream: ReadableStream<Uint8Array>;
    close(): Promise<void>;
}

export interface QueryParameter {
    readonly name: string;
    readonly value: unknown;
    readonly style: string;
    readonly explode: boolean;
}

export interface OperationRequest<T> {
    readonly operationId: string;
    readonly method: string;
    /** Relative unencoded template; transport substitutes/encodes pathParams once. */
    readonly path: string;
    readonly pathParams: Readonly<Record<string, string>>;
    readonly query: readonly QueryParameter[];
    readonly headers: Readonly<Record<string, unknown>>;
    readonly authenticated: boolean;
    /** Public transport inserts its constructor-owned key after schema encoding. */
    readonly bodyKey?: string;
    readonly audience: 'management' | 'public' | 'server';
    readonly body?: unknown;
    readonly bodySchema?: Schema;
    readonly contentType?: string;
    readonly form?: Readonly<Record<string, unknown>>;
    readonly response: 'json' | 'empty' | 'stream';
    readonly responseSchema?: Schema;
    /** JSON response conversion receives raw text, never native JSON.parse output. */
    readonly decode: (text: string) => T;
}

/** Generated operations share a native, no-replay transport. */
export interface Transport {
    request<T>(request: OperationRequest<T>, options?: RequestOptions): Promise<T>;
}

export class PaginationError extends Error {
    constructor() {
        super('Server repeated a pagination cursor');
        this.name = 'PaginationError';
    }
}

export class Resource {
    protected readonly scope: Readonly<Record<string, string>>;
    constructor(protected readonly transport: Transport, scope: Readonly<Record<string, string>> = {}) {
        this.scope = Object.freeze({ ...scope });
    }
}
