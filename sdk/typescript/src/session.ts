import type { TokenPair } from './models/TokenPair.js';
import { SessionError } from './errors.js';

export interface SessionOptions { readonly onRotate?: (pair: Readonly<TokenPair>) => void | Promise<void>; }
type Refresh = (refreshToken: string) => Promise<TokenPair>;

/** One owner per login session; local single-flight is not cross-tab coordination. */
export class Session {
    #pair: Readonly<TokenPair>;
    #expiresAt = 0;
    #lifetime = 0;
    #pending?: Promise<string>;
    #invalid = false;
    #origin?: string;
    #onRotate?: SessionOptions['onRotate'];
    private constructor(pair: TokenPair, options: SessionOptions) {
        this.#pair = this.#accept(pair);
        this.#onRotate = options.onRotate;
    }
    static fromTokenPair(pair: TokenPair, options: SessionOptions = {}): Session { return new Session(pair, options); }
    #accept(pair: TokenPair): Readonly<TokenPair> {
        if (!pair.accessToken?.trim() || !pair.refreshToken?.trim() || !Number.isFinite(pair.expiresIn) || pair.expiresIn <= 0) throw new SessionError();
        this.#lifetime = pair.expiresIn * 1000;
        this.#expiresAt = Date.now() + this.#lifetime;
        return Object.freeze({ ...pair });
    }
    /** Internal transport boundary. Bind an owner to one instance to prevent credential leakage. */
    async accessToken(origin: string, refresh: Refresh): Promise<string> {
        if (this.#invalid) throw new SessionError();
        if (this.#origin !== undefined && this.#origin !== origin) throw new TypeError('Session belongs to another instance');
        this.#origin = origin;
        if (this.#pending) return this.#pending;
        if (this.#expiresAt - Date.now() >= Math.min(30_000, this.#lifetime / 10)) return this.#pair.accessToken;
        this.#pending = (async () => {
            try {
                const pair = this.#accept(await refresh(this.#pair.refreshToken));
                await this.#onRotate?.(pair);
                this.#pair = pair;
                return pair.accessToken;
            } catch {
                this.#invalid = true;
                throw new SessionError();
            }
        })();
        try { return await this.#pending; }
        finally { this.#pending = undefined; }
    }
}
