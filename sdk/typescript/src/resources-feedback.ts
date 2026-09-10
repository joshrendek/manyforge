// Generated resource tree. Do not edit.
import type { Transport } from './transport.js';
import { FeedbackPostsApi } from './apis/FeedbackPostsApi.js';
export * from './apis/FeedbackPostsApi.js';

export type FeedbackResources = { readonly posts: Readonly<FeedbackPostsApi> };

export function createFeedbackResources(transport: Transport, publishableKey: string): FeedbackResources {
    if (!publishableKey) throw new TypeError('A nonempty scope identifier is required');
    const scope = Object.freeze({ key: publishableKey });
    return Object.freeze({ posts: Object.freeze(new FeedbackPostsApi(transport, scope)) });
}
