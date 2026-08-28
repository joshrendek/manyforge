import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { MailingSendingProfile } from '../../core/mailing.service';
import { MailingSendingProfileComponent } from './sending-profile';

const business = {
  id: 'b1',
  parent_id: null,
  tenant_root_id: 'b1',
  name: 'Acme',
  status: 'active',
  is_tenant_root: true,
};

const verifiedDomain = {
  id: 'd1',
  business_id: 'b1',
  tenant_root_id: 'b1',
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
  id: 'sp1',
  business_id: 'b1',
  tenant_root_id: 'b1',
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
    const profileRequest = http.expectOne('/api/v1/businesses/b1/mailing/sending-profile');
    if (current) profileRequest.flush(current);
    else profileRequest.flush(null, { status: 404, statusText: 'Not Found' });
    http.expectOne('/api/v1/businesses/b1/email-domains').flush({
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
    component.emailDomainId = 'd1';
    component.postalAddress = '123 Main St';
    component.save();

    const request = http.expectOne('/api/v1/businesses/b1/mailing/sending-profile');
    expect(request.request.method).toBe('PUT');
    expect(request.request.body).toEqual({
      mode: 'relay',
      from_email: 'news@example.test',
      from_name: 'Acme News',
      reply_to: null,
      postal_address: '123 Main St',
      email_domain_id: 'd1',
    });
    request.flush({ ...profile, mode: 'relay', email_domain_id: 'd1', has_credentials: false });
  });

  it('verifies the profile and sends a test only after verification succeeds', () => {
    load();
    component.verify();
    const verify = http.expectOne('/api/v1/businesses/b1/mailing/sending-profile/verify');
    expect(verify.request.method).toBe('POST');
    verify.flush({ ...profile, status: 'verified' });

    component.testEmail = 'operator@example.test';
    component.sendTest();
    const test = http.expectOne('/api/v1/businesses/b1/mailing/sending-profile/test-send');
    expect(test.request.body).toEqual({ to: 'operator@example.test' });
    test.flush(null);
  });
});
