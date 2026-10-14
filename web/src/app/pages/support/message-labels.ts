import { DnsRecordState, DroppedAttachmentReason } from '../../core/ticket.service';

// droppedReasonLabel says why an inbound attachment was not stored. Drops
// recorded before the API carried a reason (or with a reason this client does
// not know) fall back to a plain "not stored".
export function droppedReasonLabel(reason: DroppedAttachmentReason | string | null | undefined): string {
  switch (reason) {
    case 'disallowed_type':
      return 'not stored: file type not allowed';
    case 'too_large':
      return 'not stored: too large';
    case 'limit_exceeded':
      return 'not stored: attachment limit reached';
    case 'storage_disabled':
      return 'not stored: attachment storage is off';
    default:
      return 'not stored';
  }
}

export type AuthMechanism = 'spf' | 'dkim' | 'dmarc';

// authVerdictLabel renders a message authentication verdict. "none" means the
// sender's domain publishes nothing to check against, which is not the same as
// a failure or an unreadable result, so it gets its own wording per mechanism.
export function authVerdictLabel(mechanism: AuthMechanism, state: DnsRecordState): string {
  if (state !== 'none') return state;
  switch (mechanism) {
    case 'spf':
      return 'no record';
    case 'dkim':
      return 'unsigned';
    case 'dmarc':
      return 'no policy';
  }
}

// authVerdictTitle is the hover text for a "none" verdict; other states need none.
export function authVerdictTitle(mechanism: AuthMechanism, state: DnsRecordState): string | null {
  if (state !== 'none') return null;
  switch (mechanism) {
    case 'spf':
      return "The sender's domain publishes no SPF record.";
    case 'dkim':
      return 'The message carries no DKIM signature.';
    case 'dmarc':
      return "The sender's domain publishes no DMARC policy.";
  }
}
