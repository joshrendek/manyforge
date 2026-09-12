// Generated resource tree. Do not edit.
import type { Transport } from './transport.js';
import { RootAccountApi } from './apis/RootAccountApi.js';
export * from './apis/RootAccountApi.js';
import { RootAnalyticsApi } from './apis/RootAnalyticsApi.js';
export * from './apis/RootAnalyticsApi.js';
import { RootAuthApi } from './apis/RootAuthApi.js';
export * from './apis/RootAuthApi.js';
import { RootBusinessesApi } from './apis/RootBusinessesApi.js';
export * from './apis/RootBusinessesApi.js';
import { RootGithubAppInstallUrlApi } from './apis/RootGithubAppInstallUrlApi.js';
export * from './apis/RootGithubAppInstallUrlApi.js';
import { RootGithubAppInstallationsApi } from './apis/RootGithubAppInstallationsApi.js';
export * from './apis/RootGithubAppInstallationsApi.js';
import { RootInvitationsApi } from './apis/RootInvitationsApi.js';
export * from './apis/RootInvitationsApi.js';
import { RootMailingReportingApi } from './apis/RootMailingReportingApi.js';
export * from './apis/RootMailingReportingApi.js';
import { RootPermissionsApi } from './apis/RootPermissionsApi.js';
export * from './apis/RootPermissionsApi.js';
import { RootTenantMergesApi } from './apis/RootTenantMergesApi.js';
export * from './apis/RootTenantMergesApi.js';
import { RootTenantMergesOptionsApi } from './apis/RootTenantMergesOptionsApi.js';
export * from './apis/RootTenantMergesOptionsApi.js';

export type RootResources = { readonly account: Readonly<RootAccountApi>; readonly analytics: Readonly<RootAnalyticsApi>; readonly auth: Readonly<RootAuthApi>; readonly businesses: Readonly<RootBusinessesApi>; readonly githubApp: { readonly installUrl: Readonly<RootGithubAppInstallUrlApi>; readonly installations: Readonly<RootGithubAppInstallationsApi> }; readonly invitations: Readonly<RootInvitationsApi>; readonly mailing: { readonly reporting: Readonly<RootMailingReportingApi> }; readonly permissions: Readonly<RootPermissionsApi>; readonly tenantMerges: Readonly<RootTenantMergesApi> & { readonly options: Readonly<RootTenantMergesOptionsApi> } };

export function createRootResources(transport: Transport): RootResources {
    const scope = Object.freeze({});
    return Object.freeze({ account: Object.freeze(new RootAccountApi(transport, scope)), analytics: Object.freeze(new RootAnalyticsApi(transport, scope)), auth: Object.freeze(new RootAuthApi(transport, scope)), businesses: Object.freeze(new RootBusinessesApi(transport, scope)), githubApp: Object.freeze({ installUrl: Object.freeze(new RootGithubAppInstallUrlApi(transport, scope)), installations: Object.freeze(new RootGithubAppInstallationsApi(transport, scope)) }), invitations: Object.freeze(new RootInvitationsApi(transport, scope)), mailing: Object.freeze({ reporting: Object.freeze(new RootMailingReportingApi(transport, scope)) }), permissions: Object.freeze(new RootPermissionsApi(transport, scope)), tenantMerges: Object.freeze(Object.assign(new RootTenantMergesApi(transport, scope), { options: Object.freeze(new RootTenantMergesOptionsApi(transport, scope)) })) });
}
