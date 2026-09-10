import { isLosslessNumber, parse, stringify } from 'lossless-json';
import { propertyNames, schemas } from './schemas.js';

export type JsonValue = null | boolean | string | number | bigint | JsonValue[] | { [key: string]: JsonValue };
export type Direction = 'decode' | 'encode';
export interface Schema {
    $ref?: string;
    type?: string;
    format?: string;
    nullable?: boolean;
    properties?: Record<string, Schema>;
    required?: string[];
    items?: Schema;
    additionalProperties?: boolean | Schema;
    enum?: (string | number | boolean | null)[];
    oneOf?: Schema[];
    anyOf?: Schema[];
    allOf?: Schema[];
    [annotation: string]: unknown;
}

export class InvalidPayloadError extends Error {
    constructor(message = 'Payload does not match the declared response schema') {
        super(message);
        this.name = 'InvalidPayloadError';
    }
}

function object(value: unknown): value is Record<string, unknown> {
    return typeof value === 'object' && value !== null && !Array.isArray(value) && !isLosslessNumber(value);
}

function arbitrary(value: unknown): JsonValue {
    if (isLosslessNumber(value)) {
        const text = value.toString();
        if (/^-?\d+$/.test(text)) {
            const integer = BigInt(text);
            return integer > BigInt(Number.MAX_SAFE_INTEGER) || integer < BigInt(Number.MIN_SAFE_INTEGER) ? integer : Number(integer);
        }
        const number = Number(text);
        if (!Number.isFinite(number)) throw new InvalidPayloadError('JSON number exceeds the supported floating-point range');
        return number;
    }
    if (value === null || typeof value === 'string' || typeof value === 'boolean' || typeof value === 'bigint') return value;
    if (typeof value === 'number' && Number.isFinite(value)) return value;
    if (Array.isArray(value)) return value.map(arbitrary);
    if (object(value)) return Object.fromEntries(Object.entries(value).filter(([, child]) => child !== undefined).map(([key, child]) => [key, arbitrary(child)]));
    throw new InvalidPayloadError('Expected a JSON-compatible value');
}

function dereference(schema: Schema): Schema {
    if (!schema.$ref) return schema;
    const name = schema.$ref.slice('#/components/schemas/'.length);
    const target = schemas[name];
    if (!target) throw new InvalidPayloadError('Unresolved generated schema reference');
    return target;
}

// Known discriminator constants choose the best union branch without making
// extensible enum fields reject newly introduced server values.
function unionScore(schema: Schema, value: unknown, direction: Direction): number {
    schema = dereference(schema);
    if (!object(value)) return 0;
    let score = 0;
    for (const [wire, child] of Object.entries(schema.properties ?? {})) {
        const key = direction === 'decode' ? wire : propertyNames[wire] ?? wire;
        if (child.enum?.includes(value[key] as string)) score += 2;
        if (value[key] !== undefined) score++;
    }
    return score;
}

export function convert(schema: Schema, value: unknown, direction: Direction): unknown {
    schema = dereference(schema);
    if (value === undefined) return undefined;
    if (value === null) {
        if (schema.nullable || (!schema.type && !schema.oneOf && !schema.anyOf && !schema.allOf)) return null;
        throw new InvalidPayloadError('Non-nullable field was null');
    }
    const alternatives = schema.oneOf ?? schema.anyOf;
    if (alternatives) {
        const ordered = [...alternatives].sort((left, right) => unionScore(right, value, direction) - unionScore(left, value, direction));
        for (const alternative of ordered) {
            try { return convert(alternative, value, direction); }
            catch (error) { if (!(error instanceof InvalidPayloadError)) throw error; }
        }
        throw new InvalidPayloadError('Value does not match any declared union variant');
    }
    if (schema.allOf) {
        // Convert the merged shape once so unknown-field preservation cannot
        // reintroduce wire names already converted by another parent.
        const merged: Schema = { type: 'object', properties: {}, required: [] };
        for (const part of schema.allOf) {
            const resolved = dereference(part);
            Object.assign(merged.properties!, resolved.properties);
            merged.required!.push(...resolved.required ?? []);
        }
        return convert(merged, value, direction);
    }
    if (schema.type === 'array') {
        if (!Array.isArray(value)) throw new InvalidPayloadError('Expected an array');
        return value.map(child => convert(schema.items ?? {}, child, direction));
    }
    if (schema.type === 'object' || schema.properties) {
        if (!object(value)) throw new InvalidPayloadError('Expected an object');
        const properties = schema.properties ?? {};
        const result: Record<string, unknown> = Object.create(null);
        const known = new Set<string>();
        for (const [wire, child] of Object.entries(properties)) {
            const name = propertyNames[wire] ?? wire;
            const source = direction === 'decode' ? wire : name;
            known.add(source);
            if (value[source] === undefined) {
                if (schema.required?.includes(wire)) throw new InvalidPayloadError('Required field is absent');
                continue;
            }
            result[direction === 'decode' ? name : wire] = convert(child, value[source], direction);
        }
        for (const [key, child] of Object.entries(value)) {
            if (!known.has(key) && child !== undefined) {
                result[key] = typeof schema.additionalProperties === 'object'
                    ? convert(schema.additionalProperties, child, direction) : arbitrary(child);
            }
        }
        return result;
    }
    if (schema.type === 'integer' && schema.format === 'int64') {
        if (typeof value === 'bigint') return value;
        if (isLosslessNumber(value) && /^-?\d+$/.test(value.toString())) return BigInt(value.toString());
        if (direction === 'decode' && typeof value === 'number' && Number.isSafeInteger(value)) return BigInt(value);
        throw new InvalidPayloadError('Expected a lossless int64 numeric value');
    }
    if (schema.type === 'integer' || schema.type === 'number') {
        if (!isLosslessNumber(value) && typeof value !== 'number') throw new InvalidPayloadError('Expected a numeric value');
        const number = Number(value.toString());
        if (!Number.isFinite(number) || (schema.type === 'integer' && !Number.isSafeInteger(number))) throw new InvalidPayloadError('Numeric value is out of range');
        return number;
    }
    if (schema.type === 'string') {
        if (typeof value !== 'string') throw new InvalidPayloadError('Expected a string');
        return value;
    }
    if (schema.type === 'boolean') {
        if (typeof value !== 'boolean') throw new InvalidPayloadError('Expected a boolean');
        if (schema.enum && !schema.enum.includes(value)) throw new InvalidPayloadError('Boolean value does not match the declared constant');
        return value;
    }
    return arbitrary(value);
}

export function convertModel(name: string, value: unknown, direction: Direction): unknown {
    return convert({ $ref: `#/components/schemas/${name}` }, value, direction);
}

/** Never feed native JSON.parse output here: numeric tokens must remain lossless. */
export function decodeJSON<T>(text: string, schema: Schema): T {
    try {
        return convert(schema, parse(text), 'decode') as T;
    } catch (error) {
        if (error instanceof InvalidPayloadError) throw error;
        throw new InvalidPayloadError('Response body is not valid JSON');
    }
}

/** Stable schema-field ordering; bigint values are emitted as numeric JSON tokens. */
export function encodeJSON(value: unknown, schema: Schema): string {
    const text = stringify(convert(schema, value, 'encode'));
    if (text === undefined) throw new InvalidPayloadError('Expected a JSON body');
    return text;
}
