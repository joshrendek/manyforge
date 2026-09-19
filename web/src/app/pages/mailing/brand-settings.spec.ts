import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { Component, Input } from '@angular/core';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { By } from '@angular/platform-browser';
import { provideRouter } from '@angular/router';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import {
  MailingBrand,
  MailingBrandInput,
  MailingSendingProfile,
  MailingSetup,
} from '../../core/mailing.service';
import { MAILING_BRAND_DEFAULT_COLORS, MailingBrandSettingsComponent } from './brand-settings';
import { MailingContentDraft } from './content-editor';
import { MailingPreviewKind, MailingPreviewPaneComponent } from './preview-pane';

@Component({ selector: 'app-mailing-preview-pane', standalone: true, template: '' })
class PreviewStub {
  @Input() businessId = '';
  @Input() kind: MailingPreviewKind = 'campaigns';
  @Input() content!: MailingContentDraft;
  @Input() fromName: string | null = null;
  @Input() postalAddress: string | null = null;
  @Input() brand: MailingBrandInput | null = null;
}

const BUSINESS_ID = '11111111-1111-4111-8111-111111111111';
const BRAND_ID = '22222222-2222-4222-8222-222222222222';
const BASE = `/api/v1/businesses/${BUSINESS_ID}/mailing`;
const BRAND_URL = `${BASE}/brand`;

const business = {
  id: BUSINESS_ID,
  parent_id: null,
  tenant_root_id: BUSINESS_ID,
  name: 'Acme',
  status: 'active',
  is_tenant_root: true,
};

function setupWith(blobStorage: 'ready' | 'blocked'): MailingSetup {
  return {
    checks: [
      {
        id: 'mailing_key',
        label: 'Mailing key',
        status: 'ready',
        message: 'Available',
        action: '',
        required_for: ['relay', 'resend', 'ses'],
      },
    ],
    logo_storage: {
      ready: blobStorage === 'ready',
      message: 'Brand logo uploads require object storage.',
      action: blobStorage === 'ready' ? '' : 'Set MANYFORGE_BLOB_URL.',
    },
  };
}

const profile: MailingSendingProfile = {
  id: '33333333-3333-4333-8333-333333333333',
  business_id: BUSINESS_ID,
  tenant_root_id: BUSINESS_ID,
  mode: 'relay',
  from_email: 'news@example.test',
  from_name: 'Acme News',
  reply_to: null,
  postal_address: '1 Main St',
  email_domain_id: null,
  ses_region: null,
  ses_configuration_set: null,
  sns_topic_arn: null,
  status: 'verified',
  last_verified_at: null,
  verify_error: null,
  feedback_status: 'ready',
  feedback_error: null,
  feedback_confirmed_at: null,
  has_credentials: false,
  created_at: '2026-08-28T10:00:00Z',
  updated_at: '2026-08-28T10:00:00Z',
};

const brand: MailingBrand = {
  id: BRAND_ID,
  business_id: BUSINESS_ID,
  tenant_root_id: BUSINESS_ID,
  name: 'Acme',
  logo: null,
  logo_width: 160,
  colors: { ...MAILING_BRAND_DEFAULT_COLORS, accent: '#c9501d' },
  font_stack: 'serif',
  footer_markdown: 'Thanks for reading',
  created_at: '2026-08-28T10:00:00Z',
  updated_at: '2026-08-28T10:00:00Z',
};

describe('MailingBrandSettingsComponent', () => {
  let fixture: ComponentFixture<MailingBrandSettingsComponent>;
  let component: MailingBrandSettingsComponent;
  let http: HttpTestingController;

  function load(current: MailingBrand | null, setup = setupWith('ready')): void {
    fixture = TestBed.createComponent(MailingBrandSettingsComponent);
    component = fixture.componentInstance;
    fixture.detectChanges();
    http.expectOne('/api/v1/businesses').flush({ items: [business] });
    http.expectOne(`${BASE}/setup`).flush(setup);
    const brandRequest = http.expectOne(BRAND_URL);
    if (current) brandRequest.flush(current);
    else brandRequest.flush(null, { status: 404, statusText: 'Not Found' });
    http.expectOne(`${BASE}/sending-profile`).flush(profile);
    fixture.detectChanges();
  }

  function query<T extends HTMLElement>(testId: string): T | null {
    return fixture.nativeElement.querySelector(`[data-testid="${testId}"]`);
  }

  beforeEach(() => {
    localStorage.clear();
    TestBed.configureTestingModule({
      providers: [provideHttpClient(), provideHttpClientTesting(), provideRouter([])],
    });
    TestBed.overrideComponent(MailingBrandSettingsComponent, {
      remove: { imports: [MailingPreviewPaneComponent] },
      add: { imports: [PreviewStub] },
    });
    http = TestBed.inject(HttpTestingController);
  });

  afterEach(() => {
    fixture?.destroy();
    http.verify();
    localStorage.clear();
  });

  it('starts from the defaults and the sender name when no brand exists, then creates it', async () => {
    load(null);
    expect(component.brand()).toBeNull();
    expect(component.form().colors).toEqual(MAILING_BRAND_DEFAULT_COLORS);
    expect(component.form().font_stack).toBe('system');
    // ngModel writes the prefilled sender name to the DOM on the microtask queue.
    await fixture.whenStable();
    expect(query<HTMLInputElement>('brand-name')?.value).toBe('Acme News');
    expect(query<HTMLInputElement>('brand-logo-file')?.disabled).toBe(true);
    expect(query('brand-logo-blocker')?.textContent).toContain('Save the brand');
    expect(query('brand-delete')).toBeNull();
    expect(component.canSave()).toBe(true);

    component.patch({ footer_markdown: 'Thanks' });
    component.save();
    const request = http.expectOne(BRAND_URL);
    expect(request.request.method).toBe('PUT');
    expect(request.request.body).toEqual({
      name: 'Acme News',
      colors: MAILING_BRAND_DEFAULT_COLORS,
      font_stack: 'system',
      footer_markdown: 'Thanks',
    });
    request.flush({ ...brand, name: 'Acme News', footer_markdown: 'Thanks' });
    fixture.detectChanges();
    expect(component.brand()?.id).toBe(BRAND_ID);
    expect(component.dirty()).toBe(false);
    expect(component.canSave()).toBe(false);
    expect(query<HTMLInputElement>('brand-logo-file')?.disabled).toBe(false);
    expect(query('brand-logo-blocker')).toBeNull();
  });

  it('normalizes loose hex input and blocks saving while a color is invalid', () => {
    load(brand);
    expect(component.canSave()).toBe(false);
    component.patchColor('accent', ' 1769AA ');
    expect(component.form().colors.accent).toBe('#1769aa');
    expect(component.canSave()).toBe(true);

    component.patchColor('accent', '#12');
    fixture.detectChanges();
    expect(component.canSave()).toBe(false);
    expect(query<HTMLButtonElement>('brand-save')?.disabled).toBe(true);
    expect(query('brand-color-error-accent')).toBeTruthy();
    expect(query<HTMLInputElement>('brand-color-accent')?.value).toBe('#000000');
    component.save();
    http.expectNone(BRAND_URL);
    // The live preview drops the invalid color so the server falls back to its default.
    expect(component.previewBrand().colors.accent).toBe('');
    expect(component.previewBrand().colors.background).toBe('#f4f6f8');
  });

  it('feeds the unsaved brand into the preview pane as an override', () => {
    load(brand);
    component.patch({ name: 'Acme Labs', font_stack: 'mono' });
    fixture.detectChanges();
    const preview = fixture.debugElement.query(By.directive(PreviewStub))
      .componentInstance as PreviewStub;
    expect(preview.kind).toBe('templates');
    expect(preview.fromName).toBe('Acme News');
    expect(preview.postalAddress).toBe('1 Main St');
    expect(preview.brand).toMatchObject({ name: 'Acme Labs', font_stack: 'mono' });
    expect(preview.content.body_markdown).toContain('Welcome aboard');
  });

  it('disables the logo uploader when blob storage is blocked', () => {
    load(brand, setupWith('blocked'));
    expect(query<HTMLInputElement>('brand-logo-file')?.disabled).toBe(true);
    expect(query('brand-logo-blocker')?.textContent).toContain('Set MANYFORGE_BLOB_URL.');
    component.uploadLogo(new Blob(['png'], { type: 'image/png' }));
    http.expectNone(`${BRAND_URL}/logo`);
  });

  it('uploads the logo as a multipart file part and keeps unsaved edits', () => {
    load(brand);
    component.patch({ name: 'Acme Labs' });
    const file = new File(['png-bytes'], 'logo.png', { type: 'image/png' });
    component.uploadLogo(file);
    const upload = http.expectOne(`${BRAND_URL}/logo`);
    expect(upload.request.method).toBe('PUT');
    expect(upload.request.body).toBeInstanceOf(FormData);
    expect((upload.request.body as FormData).get('file')).toBe(file);
    expect(upload.request.headers.has('Content-Type')).toBe(false);
    upload.flush({
      ...brand,
      logo: { url: 'https://mail.example.test/m/b/brand/logo?v=1', content_type: 'image/png' },
      logo_width: 240,
    });
    fixture.detectChanges();
    expect(component.brand()?.logo?.url).toContain('/m/b/brand/logo');
    expect(component.form().logo_width).toBe(240);
    expect(component.form().name).toBe('Acme Labs');
    expect(query<HTMLImageElement>('brand-logo-image')?.getAttribute('src')).toContain(
      '/m/b/brand/logo',
    );

    component.removeLogo();
    const remove = http.expectOne(`${BRAND_URL}/logo`);
    expect(remove.request.method).toBe('DELETE');
    remove.flush({ ...brand, logo: null });
    fixture.detectChanges();
    expect(component.brand()?.logo).toBeNull();
    expect(query('brand-logo-image')).toBeNull();
  });

  it('rejects oversized logos before contacting the server', () => {
    load(brand);
    component.uploadLogo(new Blob([new Uint8Array(512 * 1024 + 1)], { type: 'image/png' }));
    http.expectNone(`${BRAND_URL}/logo`);
  });

  it('deletes the brand after confirmation and returns to the defaults', () => {
    load(brand);
    query<HTMLButtonElement>('brand-delete')?.click();
    http.expectNone(BRAND_URL);
    component.confirmDelete.set(true);
    fixture.detectChanges();
    query<HTMLButtonElement>('brand-delete-confirm')?.click();
    const request = http.expectOne(BRAND_URL);
    expect(request.request.method).toBe('DELETE');
    request.flush(null, { status: 204, statusText: 'No Content' });
    fixture.detectChanges();
    expect(component.brand()).toBeNull();
    expect(component.form()).toEqual({
      name: 'Acme News',
      colors: MAILING_BRAND_DEFAULT_COLORS,
      font_stack: 'system',
      footer_markdown: '',
    });
    expect(query<HTMLInputElement>('brand-logo-file')?.disabled).toBe(true);
  });
});
