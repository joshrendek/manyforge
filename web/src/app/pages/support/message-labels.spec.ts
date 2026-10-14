import { describe, expect, it } from 'vitest';
import { TicketMessage } from '../../core/ticket.service';
import { authFlag, droppedReasonLabel } from './message-labels';

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

function msg(over: Partial<TicketMessage>): TicketMessage {
  return {
    id: 'm1', ticket_id: 't1', direction: 'inbound', message_id: null, in_reply_to: null, references: [],
    author_principal_id: null, body_text: null, body_html: null, attachments: [], dropped_attachments: [],
    spf_result: 'unknown', dkim_result: 'unknown', dmarc_result: 'unknown', created_at: '2026-10-10T00:00:00Z',
    ...over,
  };
}

describe('authFlag', () => {
  it('falls back to the *_result fields without auth_verdicts', () => {
    const m = msg({ spf_result: 'pass', dkim_result: 'fail', dmarc_result: 'unknown' });
    expect(authFlag(m, 'spf')).toEqual({ label: 'pass', cls: 'pass', title: null });
    expect(authFlag(m, 'dkim')).toEqual({ label: 'fail', cls: 'fail', title: null });
    expect(authFlag(m, 'dmarc')).toEqual({ label: 'unknown', cls: 'unknown', title: null });
    expect(authFlag(msg({ auth_verdicts: null }), 'dmarc').label).toBe('unknown');
  });

  it('prefers auth_verdicts and words "none" per mechanism, distinct from unknown', () => {
    const m = msg({ auth_verdicts: { spf: 'none', dkim: 'none', dmarc: 'none' } });
    expect(authFlag(m, 'dmarc')).toEqual({ label: 'no policy', cls: 'none', title: "The sender's domain publishes no DMARC policy." });
    expect(authFlag(m, 'spf').label).toBe('no record');
    expect(authFlag(m, 'dkim').label).toBe('unsigned');
  });

  it('shows other recorded verdicts verbatim and falls back per missing mechanism', () => {
    const m = msg({ spf_result: 'pass', auth_verdicts: { dkim: 'softfail' } });
    expect(authFlag(m, 'dkim')).toEqual({ label: 'softfail', cls: 'softfail', title: null });
    expect(authFlag(m, 'spf').label).toBe('pass');
  });
});
