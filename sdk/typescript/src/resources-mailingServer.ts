// Generated resource tree. Do not edit.
import type { Transport } from './transport.js';
import { MailingServerMailingEventsApi } from './apis/MailingServerMailingEventsApi.js';
export * from './apis/MailingServerMailingEventsApi.js';
import { MailingServerMailingSubscribersApi } from './apis/MailingServerMailingSubscribersApi.js';
export * from './apis/MailingServerMailingSubscribersApi.js';

export type MailingServerResources = { readonly mailing: { readonly events: Readonly<MailingServerMailingEventsApi>; readonly subscribers: Readonly<MailingServerMailingSubscribersApi> } };

export function createMailingServerResources(transport: Transport, publishableKey: string): MailingServerResources {
    if (!publishableKey) throw new TypeError('A nonempty scope identifier is required');
    const scope = Object.freeze({ key: publishableKey });
    return Object.freeze({ mailing: Object.freeze({ events: Object.freeze(new MailingServerMailingEventsApi(transport, scope)), subscribers: Object.freeze(new MailingServerMailingSubscribersApi(transport, scope)) }) });
}
