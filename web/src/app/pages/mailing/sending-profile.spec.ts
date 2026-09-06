import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { MailingSendingProfile } from '../../core/mailing.service';
import { MailingSendingProfileComponent } from './sending-profile';

const BUSINESS_ID = '11111111-1111-4111-8111-111111111111';
const DOMAIN_ID = '22222222-2222-4222-8222-222222222222';
const PROFILE_ID = '33333333-3333-4333-8333-333333333333';
const PROFILE_URL = `/api/v1/businesses/${BUSINESS_ID}/mailing/sending-profile`;

const business = {
  id: BUSINESS_ID,
  parent_id: null,
  tenant_root_id: BUSINESS_ID,
  name: 'Acme',
  status: 'active',
  is_tenant_root: true,
};

const verifiedDomain = {
  id: DOMAIN_ID,
  business_id: BUSINESS_ID,
  tenant_root_id: BUSINESS_ID,
  domain: 'example.test',
  mode: 'forward_in',
  verification: 'verified',
  verified_at: '2026-08-28T10:00:00Z',
  dkim_state: 'pass',
  spf_state: 'pass',
  dns_challenge: {
    verification_txt: { name: '_verify.example.test', value: 'value' },
    dkim_record: { name: 'mf._domainkey.example.test', value: 'dkim' },
    spf_hint: 'v=spf1 include:example.test ~all',
    mx_hint: null,
  },
  created_at: '2026-08-28T10:00:00Z',
};

const profile: MailingSendingProfile = {
  id: PROFILE_ID,
  business_id: BUSINESS_ID,
  tenant_root_id: BUSINESS_ID,
  mode: 'resend',
  from_email: 'news@example.test',
  from_name: 'Acme News',
  reply_to: null,
  postal_address: null,
  email_domain_id: null,
  ses_region: null,
  ses_configuration_set: null,
  sns_topic_arn: null,
  status: 'unverified',
  last_verified_at: null,
  verify_error: null,
  has_credentials: true,
  created_at: '2026-08-28T10:00:00Z',
  updated_at: '2026-08-28T10:00:00Z',
};

describe('MailingSendingProfileComponent', () => {
  let fixture: ComponentFixture<MailingSendingProfileComponent>;
  let component: MailingSendingProfileComponent;
  let http: HttpTestingController;

  function load(current: MailingSendingProfile | null = profile): void {
    fixture = TestBed.createComponent(MailingSendingProfileComponent);
    component = fixture.componentInstance;
    fixture.detectChanges();
    http.expectOne('/api/v1/businesses').flush({ items: [business] });
    const profileRequest = http.expectOne(PROFILE_URL);
    if (current) profileRequest.flush(current);
    else profileRequest.flush(null, { status: 404, statusText: 'Not Found' });
    http.expectOne(`/api/v1/businesses/${BUSINESS_ID}/email-domains`).flush({
      items: [verifiedDomain],
      next_cursor: null,
    });
    fixture.detectChanges();
  }

  beforeEach(() => {
    localStorage.clear();
    TestBed.configureTestingModule({
      providers: [provideHttpClient(), provideHttpClientTesting()],
    });
    http = TestBed.inject(HttpTestingController);
  });

  afterEach(() => {
    http.verify();
    localStorage.clear();
  });

  it('loads the current profile without ever rendering stored credentials', () => {
    load();
    const element: HTMLElement = fixture.nativeElement;
    expect(element.querySelector('[data-testid="sending-credentials-stored"]')).toBeTruthy();
    expect(element.querySelector('[data-testid="sending-resend-key"]')).toBeNull();
    expect(element.querySelector('[data-testid="sending-postal-warning"]')).toBeTruthy();
    expect(element.textContent).not.toContain('re_secret');
  });

  it('creates a relay profile from a verified email domain', () => {
    load(null);
    component.mode = 'relay';
    component.fromName = 'Acme News';
    component.fromEmail = 'news@example.test';
    component.emailDomainId = DOMAIN_ID;
    component.postalAddress = '123 Main St';
    component.save();

    const request = http.expectOne(PROFILE_URL);
    expect(request.request.method).toBe('PUT');
    expect(request.request.body).toEqual({
      mode: 'relay',
      from_email: 'news@example.test',
      from_name: 'Acme News',
      reply_to: null,
      postal_address: '123 Main St',
      email_domain_id: DOMAIN_ID,
    });
    request.flush({ ...profile, mode: 'relay', email_domain_id: DOMAIN_ID, has_credentials: false });
  });

  it('verifies the profile and sends a test only after verification succeeds', () => {
    load();
    component.verify();
    const verify = http.expectOne(`${PROFILE_URL}/verify`);
    expect(verify.request.method).toBe('POST');
    verify.flush({ ...profile, status: 'verified' });

    component.testEmail = 'operator@example.test';
    component.sendTest();
    const test = http.expectOne(`${PROFILE_URL}/test-send`);
    expect(test.request.body).toEqual({ to: 'operator@example.test' });
    test.flush(null);
  });
});
