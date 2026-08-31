import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { Component, Input } from '@angular/core';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { ActivatedRoute, convertToParamMap, provideRouter } from '@angular/router';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { MailingContentDraft } from './content-editor';
import { MailingCampaignEditorComponent } from './campaign-editor';
import { MailingPreviewPaneComponent, MailingPreviewKind } from './preview-pane';

@Component({ selector: 'app-mailing-preview-pane', standalone: true, template: '' })
class PreviewStub {
  @Input() businessId = '';
  @Input() kind: MailingPreviewKind = 'campaigns';
  @Input() content!: MailingContentDraft;
  @Input() fromName: string | null = null;
  @Input() postalAddress: string | null = null;
}

const list = {
  id: 'l1',
  business_id: 'b1',
  tenant_root_id: 'b1',
  slug: 'news',
  name: 'News',
  description: null,
  double_opt_in: true,
  status: 'active',
  created_at: '',
  updated_at: '',
};
const profile = {
  id: 'p1',
  business_id: 'b1',
  tenant_root_id: 'b1',
  mode: 'resend',
  from_email: 'news@acme.test',
  from_name: 'Acme',
  reply_to: null,
  postal_address: '1 Main Street',
  email_domain_id: null,
  ses_region: null,
  ses_configuration_set: null,
  sns_topic_arn: null,
  status: 'verified',
  last_verified_at: '',
  verify_error: null,
  has_credentials: true,
  created_at: '',
  updated_at: '',
};
const campaign = {
  id: 'c1',
  business_id: 'b1',
  tenant_root_id: 'b1',
  list_id: 'l1',
  profile_id: 'p1',
  name: 'Product update',
  subject: 'What is new',
  preheader: null,
  body_markdown: '# Hello',
  tag_filter: [],
  track_opens: true,
  track_clicks: true,
  status: 'draft',
  scheduled_at: null,
  started_at: null,
  completed_at: null,
  recipient_count: 0,
  sent_count: 0,
  delivered_count: 0,
  bounced_count: 0,
  complained_count: 0,
  opened_count: 0,
  clicked_count: 0,
  unsubscribed_count: 0,
  failed_count: 0,
  last_error: null,
  created_by: 'u1',
  created_at: '',
  updated_at: '',
};

describe('MailingCampaignEditorComponent', () => {
  let http: HttpTestingController;
  let fixture: ComponentFixture<MailingCampaignEditorComponent> | undefined;

  beforeEach(() => {
    localStorage.clear();
    TestBed.configureTestingModule({
      providers: [
        provideHttpClient(),
        provideHttpClientTesting(),
        provideRouter([]),
        {
          provide: ActivatedRoute,
          useValue: {
            snapshot: { paramMap: convertToParamMap({ businessId: 'b1', campaignId: 'c1' }) },
          },
        },
      ],
    });
    TestBed.overrideComponent(MailingCampaignEditorComponent, {
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

  function mount(
    campaignResponse: Record<string, unknown> = campaign,
    profileResponse: Record<string, unknown> = profile,
  ): ComponentFixture<MailingCampaignEditorComponent> {
    fixture = TestBed.createComponent(MailingCampaignEditorComponent);
    fixture.detectChanges();
    http.expectOne('/api/v1/businesses/b1/mailing/campaigns/c1').flush(campaignResponse);
    http.expectOne('/api/v1/businesses/b1/mailing/lists').flush({
      items: [list],
      next_cursor: null,
    });
    http.expectOne('/api/v1/businesses/b1/mailing/sending-profile').flush(profileResponse);
    fixture.detectChanges();
    return fixture;
  }

  it('gates saving on a verified profile and resets dirty state after save', () => {
    const editor = mount();
    expect(editor.componentInstance.hasUnsavedChanges()).toBe(false);
    editor.componentInstance.name = 'Renamed';
    expect(editor.componentInstance.hasUnsavedChanges()).toBe(true);
    editor.componentInstance.save();
    const request = http.expectOne('/api/v1/businesses/b1/mailing/campaigns/c1');
    expect(request.request.method).toBe('PATCH');
    expect(request.request.body).toMatchObject({ name: 'Renamed', body_markdown: '# Hello' });
    request.flush({ ...campaign, name: 'Renamed' });
    expect(editor.componentInstance.hasUnsavedChanges()).toBe(false);
  });

  it('disables the editable form while a save is in flight', () => {
    const editor = mount();
    editor.componentInstance.name = 'Renamed';
    editor.componentInstance.save();
    editor.detectChanges();
    expect(
      (
        editor.nativeElement.querySelector(
          '[data-testid="campaign-editor-name"]',
        ) as HTMLInputElement
      ).readOnly,
    ).toBe(true);
    expect(
      (
        editor.nativeElement.querySelector(
          '[data-testid="mailing-content-body"]',
        ) as HTMLTextAreaElement
      ).readOnly,
    ).toBe(true);
    http
      .expectOne('/api/v1/businesses/b1/mailing/campaigns/c1')
      .flush({ ...campaign, name: 'Renamed' });
  });

  it('persists visible content before sending a comma-separated test', () => {
    const editor = mount();
    editor.componentInstance.content.update((content) => ({
      ...content,
      body_markdown: '# Updated',
    }));
    editor.componentInstance.testRecipients = 'ada@example.com, ADA@example.com, grace@example.com';
    editor.componentInstance.sendTest();
    const patch = http.expectOne('/api/v1/businesses/b1/mailing/campaigns/c1');
    expect(patch.request.body.body_markdown).toBe('# Updated');
    patch.flush({ ...campaign, body_markdown: '# Updated' });
    const send = http.expectOne('/api/v1/businesses/b1/mailing/campaigns/c1/test-send');
    expect(send.request.body).toEqual({ to: ['ada@example.com', 'grace@example.com'] });
    send.flush(null);
  });

  it('requires inline confirmation and then saves before sending now', () => {
    const editor = mount();
    editor.componentInstance.beginSendConfirmation();
    http.expectNone('/api/v1/businesses/b1/mailing/campaigns/c1/send');
    editor.componentInstance.sendNow();
    http.expectOne('/api/v1/businesses/b1/mailing/campaigns/c1').flush(campaign);
    const send = http.expectOne('/api/v1/businesses/b1/mailing/campaigns/c1/send');
    expect(send.request.body).toEqual({ scheduled_at: null });
    send.flush({ ...campaign, status: 'sending' });
    expect(editor.componentInstance.campaign()?.status).toBe('sending');
  });

  it('invalidates send confirmation when the audience or content changes', () => {
    const editor = mount();
    editor.componentInstance.beginSendConfirmation();
    editor.componentInstance.tagFilter = ['different-audience'];
    editor.componentInstance.sendNow();
    http.expectNone('/api/v1/businesses/b1/mailing/campaigns/c1');
    http.expectNone('/api/v1/businesses/b1/mailing/campaigns/c1/send');
    expect(editor.componentInstance.confirmSend()).toBe(false);
  });

  it('converts a future browser-local schedule to ISO after saving', () => {
    const editor = mount();
    const local = '2030-09-01T12:30';
    editor.componentInstance.scheduleAt = local;
    editor.componentInstance.schedule();
    http.expectOne('/api/v1/businesses/b1/mailing/campaigns/c1').flush(campaign);
    const send = http.expectOne('/api/v1/businesses/b1/mailing/campaigns/c1/send');
    expect(send.request.body).toEqual({ scheduled_at: new Date(local).toISOString() });
    send.flush({ ...campaign, status: 'scheduled', scheduled_at: new Date(local).toISOString() });
    expect(editor.componentInstance.campaign()?.status).toBe('scheduled');
  });

  it('treats a typed-but-unscheduled date as unsaved navigation state', () => {
    const editor = mount();
    expect(editor.componentInstance.hasUnsavedChanges()).toBe(false);
    editor.componentInstance.scheduleAt = '2030-09-01T12:30';
    expect(editor.componentInstance.hasUnsavedChanges()).toBe(true);
  });

  it('renders an unverified profile warning and disables Save', () => {
    const editor = mount(campaign, { ...profile, status: 'unverified' });
    expect(
      editor.nativeElement.querySelector('[data-testid="campaign-profile-warning"]'),
    ).toBeTruthy();
    expect(
      (editor.nativeElement.querySelector('[data-testid="campaign-save"]') as HTMLButtonElement)
        .disabled,
    ).toBe(true);
  });

  it('cancels a scheduled campaign from its read-only state', () => {
    const editor = mount({
      ...campaign,
      status: 'scheduled',
      scheduled_at: '2026-09-01T12:00:00Z',
    });
    editor.componentInstance.cancel();
    const cancel = http.expectOne('/api/v1/businesses/b1/mailing/campaigns/c1/cancel');
    cancel.flush({ ...campaign, status: 'cancelled' });
    expect(editor.componentInstance.campaign()?.status).toBe('cancelled');
  });
});
