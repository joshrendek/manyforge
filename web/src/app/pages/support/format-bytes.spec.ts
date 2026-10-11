import { describe, expect, it } from 'vitest';
import { formatBytes } from './format-bytes';

describe('formatBytes', () => {
  it('formats bytes, KB and MB', () => {
    expect(formatBytes(24)).toBe('24 bytes');
    expect(formatBytes(1024)).toBe('1.0 KB');
    expect(formatBytes(1229)).toBe('1.2 KB');
    expect(formatBytes(3.4 * 1024 * 1024)).toBe('3.4 MB');
  });
});
