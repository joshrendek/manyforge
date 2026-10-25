// Generated resource tree. Do not edit.
import type { Transport } from './transport.js';
import { MailingServerMailingEventsApi } from './apis/MailingServerMailingEventsApi.js';
export * from './apis/MailingServerMailingEventsApi.js';
import { MailingServerMailingSubscribersApi } from './apis/MailingServerMailingSubscribersApi.js';
export * from './apis/MailingServerMailingSubscribersApi.js';
import { MailingServerSupportPortalContactApi } from './apis/MailingServerSupportPortalContactApi.js';
export * from './apis/MailingServerSupportPortalContactApi.js';
import { MailingServerSupportPortalTicketsApi } from './apis/MailingServerSupportPortalTicketsApi.js';
export * from './apis/MailingServerSupportPortalTicketsApi.js';

export type MailingServerResources = { readonly mailing: { readonly events: Readonly<MailingServerMailingEventsApi>; readonly subscribers: Readonly<MailingServerMailingSubscribersApi> }; readonly support: { readonly portal: { readonly contact: Readonly<MailingServerSupportPortalContactApi>; readonly tickets: Readonly<MailingServerSupportPortalTicketsApi> } } };

export function createMailingServerResources(transport: Transport, publishableKey: string): MailingServerResources {
    if (!publishableKey) throw new TypeError('A nonempty scope identifier is required');
    const scope = Object.freeze({ key: publishableKey });
    return Object.freeze({ mailing: Object.freeze({ events: Object.freeze(new MailingServerMailingEventsApi(transport, scope)), subscribers: Object.freeze(new MailingServerMailingSubscribersApi(transport, scope)) }), support: Object.freeze({ portal: Object.freeze({ contact: Object.freeze(new MailingServerSupportPortalContactApi(transport, scope)), tickets: Object.freeze(new MailingServerSupportPortalTicketsApi(transport, scope)) }) }) });
}
