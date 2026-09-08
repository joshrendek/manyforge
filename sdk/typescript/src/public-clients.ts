import { publicTransport, type PublicClientOptions } from './fetch-transport.js';
import { createFeedbackResources, type FeedbackResources } from './resources-feedback.js';
import { TelemetryTelemetryApi } from './apis/TelemetryTelemetryApi.js';
import { MailingMailingApi } from './apis/MailingMailingApi.js';
import { AnalyticsAnalyticsApi } from './apis/AnalyticsAnalyticsApi.js';

export interface FeedbackClient extends FeedbackResources {}
export class FeedbackClient {
    constructor(options: PublicClientOptions) {
        Object.assign(this, createFeedbackResources(publicTransport(options), options.publishableKey));
        Object.freeze(this);
    }
}
export class TelemetryClient extends TelemetryTelemetryApi {
    constructor(options: PublicClientOptions) {
        super(publicTransport(options), { key: options.publishableKey });
        Object.freeze(this);
    }
}
export class MailingClient extends MailingMailingApi {
    constructor(options: PublicClientOptions) {
        super(publicTransport(options), { key: options.publishableKey });
        Object.freeze(this);
    }
}
/** Browser origin is browser-owned. Server callers must use the server entrypoint. */
export class AnalyticsClient extends AnalyticsAnalyticsApi {
    constructor(options: PublicClientOptions) {
        if (typeof globalThis.location === 'undefined') throw new TypeError('Server analytics requires AnalyticsClient from @manyforge/sdk/server');
        super(publicTransport(options));
        Object.freeze(this);
    }
}
