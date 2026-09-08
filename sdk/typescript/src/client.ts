import { FetchTransport, type ClientOptions } from './fetch-transport.js';
import { createRootResources, type RootResources } from './resources-root.js';
import { createBusinessResources, type BusinessResources } from './resources-business.js';

export interface ManyForge extends RootResources {}
export class ManyForge {
    readonly #transport: FetchTransport;
    constructor(options: ClientOptions) {
        this.#transport = new FetchTransport(options);
        Object.assign(this, createRootResources(this.#transport));
        Object.freeze(this);
    }
    business(id: string): BusinessResources {
        return createBusinessResources(this.#transport, id);
    }
}
