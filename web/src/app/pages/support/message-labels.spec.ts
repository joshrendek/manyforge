import { describe, expect, it } from 'vitest';
import { authVerdictLabel, authVerdictTitle, droppedReasonLabel } from './message-labels';

describe('droppedReasonLabel', () => {
  it('names each drop reason', () => {
    expect(droppedReasonLabel('disallowed_type')).toBe('not stored: file type not allowed');
    expect(droppedReasonLabel('too_large')).toBe('not stored: too large');
    expect(droppedReasonLabel('limit_exceeded')).toBe('not stored: attachment limit reached');
    expect(droppedReasonLabel('storage_disabled')).toBe('not stored: attachment storage is off');
  });

  it('falls back to plain "not stored" for older drops and unknown reasons', () => {
    expect(droppedReasonLabel(undefined)).toBe('not stored');
    expect(droppedReasonLabel(null)).toBe('not stored');
    expect(droppedReasonLabel('something_new')).toBe('not stored');
  });
});

describe('authVerdictLabel', () => {
  it('passes ordinary verdicts through', () => {
    for (const s of ['pass', 'fail', 'pending', 'unknown'] as const) {
      expect(authVerdictLabel('dmarc', s)).toBe(s);
      expect(authVerdictTitle('dmarc', s)).toBeNull();
    }
  });

  it('words "none" per mechanism, distinct from unknown', () => {
    expect(authVerdictLabel('dmarc', 'none')).toBe('no policy');
    expect(authVerdictLabel('spf', 'none')).toBe('no record');
    expect(authVerdictLabel('dkim', 'none')).toBe('unsigned');
    expect(authVerdictTitle('dmarc', 'none')).toContain('no DMARC policy');
  });
});
