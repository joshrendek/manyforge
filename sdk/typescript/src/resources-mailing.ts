// Generated resource tree. Do not edit.
import type { Transport } from './transport.js';
import { MailingMailingApi } from './apis/MailingMailingApi.js';
export * from './apis/MailingMailingApi.js';

export type MailingResources = { readonly mailing: Readonly<MailingMailingApi> };

export function createMailingResources(transport: Transport, publishableKey: string): MailingResources {
    if (!publishableKey) throw new TypeError('A nonempty scope identifier is required');
    const scope = Object.freeze({ key: publishableKey });
    return Object.freeze({ mailing: Object.freeze(new MailingMailingApi(transport, scope)) });
}
