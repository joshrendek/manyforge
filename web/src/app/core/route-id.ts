const canonicalUUIDPattern = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

export function routeSegmentUUID(value: string): string {
  if (!canonicalUUIDPattern.test(value)) {
    throw new Error('Invalid UUID route segment');
  }
  return encodeURIComponent(value);
}
