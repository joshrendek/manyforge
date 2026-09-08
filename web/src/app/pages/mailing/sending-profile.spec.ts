import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { provideRouter } from '@angular/router';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { MailingSendingProfile, MailingSetup } from '../../core/mailing.service';
import { MailingSendingProfileComponent } from './sending-profile';

const BUSINESS_ID = '11111111-1111-4111-8111-111111111111';
const DOMAIN_ID = '22222222-2222-4222-8222-222222222222';
const PROFILE_ID = '33333333-3333-4333-8333-333333333333';
const PROFILE_URL = `/api/v1/businesses/${BUSINESS_ID}/mailing/sending-profile`;
const SETUP_URL = `/api/v1/businesses/${BUSINESS_ID}/mailing/setup`;
const readySetup: MailingSetup = {
  checks: ['outbound_enabled', 'mailing_key', 'public_url', 'smtp_relay', 'dkim_key'].map((id) => ({
    id,
    label: id,
    status: 'ready',
    message: 'Available',
    action: '',
    required_for: ['relay', 'resend', 'ses'],
  })),
};

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
  feedback_status: 'pending',
  feedback_error: null,
  feedback_confirmed_at: null,
  has_credentials: true,
  created_at: '2026-08-28T10:00:00Z',
  updated_at: '2026-08-28T10:00:00Z',
};

describe('MailingSendingProfileComponent', () => {
  let fixture: ComponentFixture<MailingSendingProfileComponent>;
  let component: MailingSendingProfileComponent;
  let http: HttpTestingController;

  function load(current: MailingSendingProfile | null = profile, setup = readySetup): void {
    fixture = TestBed.createComponent(MailingSendingProfileComponent);
    component = fixture.componentInstance;
    fixture.detectChanges();
    http.expectOne('/api/v1/businesses').flush({ items: [business] });
    http.expectOne(SETUP_URL).flush(setup);
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
      providers: [provideHttpClient(), provideHttpClientTesting(), provideRouter([])],
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
  });

  it('verifies the profile and sends a test only after verification succeeds', () => {
    load();
    component.testEmail = 'operator@example.test';
    component.sendTest();
    http.expectNone(`${PROFILE_URL}/test-send`);
    component.verify();
    const verify = http.expectOne(`${PROFILE_URL}/verify`);
    verify.flush({ ...profile, status: 'verified', feedback_status: 'ready' });

    component.testEmail = 'operator@example.test';
    component.sendTest();
    const test = http.expectOne(`${PROFILE_URL}/test-send`);
    test.flush(null);
    expect(component.testAccepted()).toBe(true);
  });

  it('does not send while provider verification succeeds but feedback remains pending', () => {
    load({ ...profile, status: 'verified' });
    component.stage.set(4);
    component.testEmail = 'operator@example.test';
    fixture.detectChanges();
    const button: HTMLButtonElement = fixture.nativeElement.querySelector(
      '[data-testid="sending-profile-test"]',
    );
    expect(button.disabled).toBe(true);
    component.sendTest();
    http.expectNone(`${PROFILE_URL}/test-send`);
  });

  it('blocks verification until applicable instance checks are ready without blocking configuration', () => {
    load(profile, {
      checks: readySetup.checks.map((check) =>
        check.id === 'outbound_enabled'
          ? {
              ...check,
              status: 'blocked',
              action: 'Ask the administrator to enable outbound mail.',
            }
          : check,
      ),
    });
    component.stage.set(3);
    fixture.detectChanges();
    const button: HTMLButtonElement = fixture.nativeElement.querySelector(
      '[data-testid="sending-profile-verify"]',
    );
    expect(button.disabled).toBe(true);
    component.verify();
    http.expectNone(`${PROFILE_URL}/verify`);
    expect(component.canSave()).toBe(true);
    component.refreshSetup();
    http.expectOne(SETUP_URL).flush(readySetup);
    component.verify();
    http
      .expectOne(`${PROFILE_URL}/verify`)
      .flush({ ...profile, status: 'verified', feedback_status: 'ready' });
  });

  it('requires saving edited fields before verification or test delivery', () => {
    load({ ...profile, status: 'verified', feedback_status: 'ready' });
    component.save();
    http.expectNone(PROFILE_URL);
    component.fromEmail = 'changed@example.test';
    component.testEmail = 'operator@example.test';
    component.verify();
    component.sendTest();
    http.expectNone(`${PROFILE_URL}/verify`);
    http.expectNone(`${PROFILE_URL}/test-send`);
    component.save();
    http.expectOne(PROFILE_URL).flush({ ...profile, from_email: 'changed@example.test' });
    component.verify();
    http.expectOne(`${PROFILE_URL}/verify`).flush({
      ...profile,
      from_email: 'changed@example.test',
      status: 'verified',
      feedback_status: 'ready',
    });
    component.sendTest();
    http.expectOne(`${PROFILE_URL}/test-send`).flush(null);
    expect(component.testAccepted()).toBe(true);
  });

  it('explains the missing mailing key without requesting unavailable profile routes', () => {
    fixture = TestBed.createComponent(MailingSendingProfileComponent);
    component = fixture.componentInstance;
    fixture.detectChanges();
    http.expectOne('/api/v1/businesses').flush({ items: [business] });
    http.expectOne(SETUP_URL).flush({
      checks: readySetup.checks.map((check) =>
        check.id === 'mailing_key'
          ? {
              ...check,
              status: 'blocked',
              action: 'Configure MANYFORGE_MAILING_MASTER_KEY and restart.',
            }
          : check,
      ),
    });
    http.expectOne(`/api/v1/businesses/${BUSINESS_ID}/email-domains`).flush({ items: [] });
    fixture.detectChanges();
    http.expectNone(PROFILE_URL);
    expect(component.error()).toBe('');
    component.stage.set(1);
    component.mode = 'resend';
    component.fromName = 'News';
    component.fromEmail = 'news@example.test';
    component.resendApiKey = 'replacement';
    expect(component.canSave()).toBe(false);
    component.refreshSetup();
    http.expectOne(SETUP_URL).flush(readySetup);
    http.expectOne(PROFILE_URL).flush(null, { status: 404, statusText: 'Not Found' });
    expect(component.canSave()).toBe(true);
  });

  it('clears test acceptance and readiness on business switches and cancels obsolete responses', () => {
    load({ ...profile, status: 'verified', feedback_status: 'ready' });
    component.testEmail = 'operator@example.test';
    component.sendTest();
    http.expectOne(`${PROFILE_URL}/test-send`).flush(null);
    expect(component.testAccepted()).toBe(true);
    component.refreshSetup();
    const oldSetup = http.expectOne(SETUP_URL);
    const secondBusiness = '44444444-4444-4444-8444-444444444444';
    const secondBase = `/api/v1/businesses/${secondBusiness}`;
    component.selectBusiness(secondBusiness);
    expect(oldSetup.cancelled).toBe(true);
    expect(component.setup()).toBeNull();
    expect(component.testAccepted()).toBe(false);
    component.sendTest();
    http.expectNone(`${secondBase}/mailing/sending-profile/test-send`);
    http.expectOne(`${secondBase}/mailing/setup`).flush(readySetup);
    const oldProfile = http.expectOne(`${secondBase}/mailing/sending-profile`);
    const oldDomains = http.expectOne(`${secondBase}/email-domains`);
    component.selectBusiness(BUSINESS_ID);
    expect(oldProfile.cancelled).toBe(true);
    expect(oldDomains.cancelled).toBe(true);
    http.expectOne(SETUP_URL).flush(readySetup);
    http.expectOne(PROFILE_URL).flush({ ...profile, status: 'verified', feedback_status: 'ready' });
    http
      .expectOne(`/api/v1/businesses/${BUSINESS_ID}/email-domains`)
      .flush({ items: [verifiedDomain] });
    component.testEmail = 'operator@example.test';
    component.sendTest();
    const oldTest = http.expectOne(`${PROFILE_URL}/test-send`);
    component.selectBusiness(secondBusiness);
    expect(oldTest.cancelled).toBe(true);
    http.expectOne(`${secondBase}/mailing/setup`).flush(readySetup);
    http
      .expectOne(`${secondBase}/mailing/sending-profile`)
      .flush(null, { status: 404, statusText: 'Not Found' });
    http.expectOne(`${secondBase}/email-domains`).flush({ items: [] });
    fixture.detectChanges();
    expect(component.profile()).toBeNull();
    expect(fixture.nativeElement.querySelector('[data-testid="sending-test-success"]')).toBeNull();
  });
});
