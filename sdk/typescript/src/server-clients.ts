import { createHmac } from 'node:crypto';
import { publicTransport, instanceOrigin, type PublicClientOptions, type SignRequest } from './fetch-transport.js';
import { createFeedbackResources, type FeedbackResources } from './resources-feedback.js';
import { createMailingServerResources, type MailingServerResources } from './resources-mailingServer.js';
import { TelemetryTelemetryApi } from './apis/TelemetryTelemetryApi.js';
import { AnalyticsAnalyticsApi } from './apis/AnalyticsAnalyticsApi.js';

export interface SignedClientOptions extends PublicClientOptions { readonly signingSecret: string; }
export interface ServerAnalyticsOptions extends PublicClientOptions { readonly sourceOrigin: string; }

function signingTransport(options: SignedClientOptions, protocol: 'feedback' | 'telemetry' | 'mailing') {
    const { signingSecret, ...publicOptions } = options;
    if (!signingSecret?.trim()) throw new TypeError('A nonempty signingSecret is required');
    const sign: SignRequest = (method, target, body) => {
        const timestamp = Math.floor(Date.now() / 1000).toString();
        const signedTarget = protocol === 'mailing' ? target.split('?')[0] : target;
        const signature = createHmac('sha256', signingSecret).update(`${timestamp}.${method}.${signedTarget}.${body}`).digest('hex');
        const headers: Record<string, string> = {};
        if (protocol === 'mailing') {
            headers['X-Mailing-Timestamp'] = timestamp;
            headers['X-Mailing-Signature'] = signature;
        } else headers[protocol === 'feedback' ? 'X-Feedback-Signature' : 'X-Telemetry-Signature'] = `t=${timestamp},v1=${signature}`;
        return headers;
    };
    return publicTransport(publicOptions, { sign });
}

export interface SignedFeedbackClient extends FeedbackResources {}
export class SignedFeedbackClient {
    constructor(options: SignedClientOptions) {
        Object.assign(this, createFeedbackResources(signingTransport(options, 'feedback'), options.publishableKey));
        Object.freeze(this);
    }
}
export class SignedTelemetryClient extends TelemetryTelemetryApi {
    constructor(options: SignedClientOptions) {
        super(signingTransport(options, 'telemetry'), { key: options.publishableKey });
        Object.freeze(this);
    }
}
export interface MailingServerClient extends MailingServerResources {}
export class MailingServerClient {
    constructor(options: SignedClientOptions) {
        Object.assign(this, createMailingServerResources(signingTransport(options, 'mailing'), options.publishableKey));
        Object.freeze(this);
    }
}
export class AnalyticsClient extends AnalyticsAnalyticsApi {
    constructor(options: ServerAnalyticsOptions) {
        if (typeof window !== 'undefined') throw new TypeError('Server analytics cannot run in a browser');
        const { sourceOrigin, ...publicOptions } = options;
        super(publicTransport(publicOptions, { sourceOrigin: instanceOrigin(sourceOrigin) }));
        Object.freeze(this);
    }
}
