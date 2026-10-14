import { DroppedAttachmentReason, TicketMessage } from '../../core/ticket.service';

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

export interface AuthFlag {
  label: string;
  // CSS modifier (the verdict token): pass / fail / none are styled; anything
  // else (unknown, softfail, ...) stays muted.
  cls: string;
  title: string | null;
}

const NONE_LABEL: Record<AuthMechanism, string> = { spf: 'no record', dkim: 'unsigned', dmarc: 'no policy' };
const NONE_TITLE: Record<AuthMechanism, string> = {
  spf: "The sender's domain publishes no SPF record.",
  dkim: 'The message carries no DKIM signature.',
  dmarc: "The sender's domain publishes no DMARC policy.",
};

// authFlag renders one message authentication verdict. The raw recorded token in
// auth_verdicts wins when present (it distinguishes "none" — nothing published to
// check against — from an unreadable result); otherwise the coarse *_result
// field is used. "none" is worded per mechanism and styled neutrally, apart
// from both fail and unknown.
export function authFlag(m: TicketMessage, mech: AuthMechanism): AuthFlag {
  const verdict = m.auth_verdicts?.[mech] || m[`${mech}_result`];
  if (verdict === 'none') return { label: NONE_LABEL[mech], cls: 'none', title: NONE_TITLE[mech] };
  return { label: verdict, cls: verdict, title: null };
}
