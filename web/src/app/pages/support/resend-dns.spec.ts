import { describe, expect, it } from 'vitest';
import { EmailDomain } from '../../core/ticket.service';
import { resendDnsChecklist } from './resend-dns';

function domain(over: Partial<EmailDomain> = {}): EmailDomain {
  return {
    id: 'ed1',
    business_id: 'b1',
    tenant_root_id: 'root',
    domain: 'support.garden.gg',
    mode: 'provider_route',
    verification: 'unverified',
    verified_at: null,
    dkim_state: 'unknown',
    spf_state: 'unknown',
    dns_challenge: {
      verification_txt: { name: '_manyforge.support.garden.gg', value: 'mf-verify=abc' },
      dkim_record: { name: 'mf1._domainkey.support.garden.gg', value: 'v=DKIM1; k=ed25519; p=x' },
      spf_hint: 'v=spf1 include:mail.manyforge.example ~all',
      mx_hint: null,
    },
    created_at: '2026-10-10T00:00:00Z',
    ...over,
  };
}

describe('resendDnsChecklist', () => {
  it('lists the ownership TXT plus every Resend record, all under the subdomain', () => {
    const rows = resendDnsChecklist(domain());
    expect(rows.map((r) => `${r.type} ${r.name}`)).toEqual([
      'TXT _manyforge.support.garden.gg',
      'MX support.garden.gg',
      'TXT resend._domainkey.support.garden.gg',
      'MX send.support.garden.gg',
      'TXT send.support.garden.gg',
    ]);
    expect(rows[0]).toMatchObject({ source: 'manyforge', value: 'mf-verify=abc' });
    expect(rows.slice(1).every((r) => r.source === 'resend')).toBe(true);
  });

  it('never proposes a record at the apex', () => {
    for (const r of resendDnsChecklist(domain())) {
      expect(r.name.endsWith('support.garden.gg')).toBe(true);
    }
  });
});
