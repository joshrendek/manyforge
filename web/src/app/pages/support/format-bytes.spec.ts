import { describe, expect, it } from 'vitest';
import { formatBytes } from './format-bytes';

describe('formatBytes', () => {
  it('formats bytes, KB and MB', () => {
    expect(formatBytes(24)).toBe('24 bytes');
    expect(formatBytes(1229)).toBe('1.2 KB');
    expect(formatBytes(3.4 * 1024 * 1024)).toBe('3.4 MB');
  });

  it('handles unit boundaries', () => {
    expect(formatBytes(0)).toBe('0 bytes');
    expect(formatBytes(1)).toBe('1 bytes');
    expect(formatBytes(1023)).toBe('1023 bytes');
    expect(formatBytes(1024)).toBe('1.0 KB');
    expect(formatBytes(1048575)).toBe('1.0 MB');
    expect(formatBytes(1048576)).toBe('1.0 MB');
  });

  it('renders an em dash for invalid input', () => {
    expect(formatBytes(NaN)).toBe('—');
    expect(formatBytes(-1)).toBe('—');
    expect(formatBytes(Infinity)).toBe('—');
  });
});
