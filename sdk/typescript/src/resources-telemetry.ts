// Generated resource tree. Do not edit.
import type { Transport } from './transport.js';
import { TelemetryTelemetryApi } from './apis/TelemetryTelemetryApi.js';
export * from './apis/TelemetryTelemetryApi.js';

export type TelemetryResources = { readonly telemetry: Readonly<TelemetryTelemetryApi> };

export function createTelemetryResources(transport: Transport, publishableKey: string): TelemetryResources {
    if (!publishableKey) throw new TypeError('A nonempty scope identifier is required');
    const scope = Object.freeze({ key: publishableKey });
    return Object.freeze({ telemetry: Object.freeze(new TelemetryTelemetryApi(transport, scope)) });
}
