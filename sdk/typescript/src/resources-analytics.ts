// Generated resource tree. Do not edit.
import type { Transport } from './transport.js';
import { AnalyticsAnalyticsApi } from './apis/AnalyticsAnalyticsApi.js';
export * from './apis/AnalyticsAnalyticsApi.js';

export type AnalyticsResources = { readonly analytics: Readonly<AnalyticsAnalyticsApi> };

export function createAnalyticsResources(transport: Transport): AnalyticsResources {
    const scope = Object.freeze({});
    return Object.freeze({ analytics: Object.freeze(new AnalyticsAnalyticsApi(transport, scope)) });
}
