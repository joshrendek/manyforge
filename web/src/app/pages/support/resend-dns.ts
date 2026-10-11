import { EmailDomain } from '../../core/ticket.service';

// One DNS record an administrator publishes for a Resend-received support domain
// (spec 017 R9). `source: 'resend'` rows show where to copy the value from,
// because Resend issues those values per account and region.
export interface DnsChecklistRow {
  purpose: string;
  type: 'TXT' | 'MX';
  name: string;
  value: string;
  source: 'manyforge' | 'resend';
  note?: string;
}

// resendDnsChecklist returns every record a provider_route domain needs. All of
// them sit at or under the support subdomain, so the apex MX and SPF are untouched.
export function resendDnsChecklist(d: EmailDomain): DnsChecklistRow[] {
  const name = d.domain;
  const copy = (what: string) => `Copy the ${what} value Resend shows under Domains → ${name}`;
  return [
    {
      purpose: 'Ownership (ManyForge)',
      type: 'TXT',
      name: d.dns_challenge.verification_txt.name,
      value: d.dns_challenge.verification_txt.value,
      source: 'manyforge',
    },
    {
      purpose: 'Receiving',
      type: 'MX',
      name,
      value: copy('receiving MX'),
      source: 'resend',
      note: 'Must be the lowest-priority MX on this name. Your apex MX is not changed.',
    },
    {
      purpose: 'DKIM',
      type: 'TXT',
      name: `resend._domainkey.${name}`,
      value: copy('DKIM'),
      source: 'resend',
    },
    {
      purpose: 'SPF return path',
      type: 'MX',
      name: `send.${name}`,
      value: copy('feedback MX'),
      source: 'resend',
    },
    {
      purpose: 'SPF return path',
      type: 'TXT',
      name: `send.${name}`,
      value: copy('SPF'),
      source: 'resend',
    },
  ];
}
