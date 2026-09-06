import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { TestBed } from '@angular/core/testing';
import { ActivatedRoute, convertToParamMap, provideRouter } from '@angular/router';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { MailingTemplateEditorComponent } from './template-editor';

const BUSINESS_ID = '11111111-1111-4111-8111-111111111111';
const TEMPLATE_ID = '22222222-2222-4222-8222-222222222222';
const TEMPLATE_URL = `/api/v1/businesses/${BUSINESS_ID}/mailing/templates/${TEMPLATE_ID}`;

const template = {
  id: TEMPLATE_ID,
  business_id: BUSINESS_ID,
  tenant_root_id: BUSINESS_ID,
  name: 'Welcome',
  subject: 'Hello',
  preheader: null,
  body_markdown: '# Hi',
  track_opens: true,
  track_clicks: true,
  created_at: '',
  updated_at: '',
};

describe('MailingTemplateEditorComponent', () => {
  let http: HttpTestingController;

  beforeEach(() => {
    TestBed.configureTestingModule({
      providers: [
        provideHttpClient(),
        provideHttpClientTesting(),
        provideRouter([
          {
            path: 'mailing/:businessId/templates/:templateId',
            component: MailingTemplateEditorComponent,
          },
        ]),
        {
          provide: ActivatedRoute,
          useValue: {
            snapshot: { paramMap: convertToParamMap({ businessId: BUSINESS_ID, templateId: TEMPLATE_ID }) },
          },
        },
      ],
    });
    http = TestBed.inject(HttpTestingController);
  });
  afterEach(() => http.verify());

  it('loads and saves Markdown and tracking settings', () => {
    const fixture = TestBed.createComponent(MailingTemplateEditorComponent);
    fixture.detectChanges();
    http.expectOne(TEMPLATE_URL).flush(template);
    fixture.detectChanges();
    expect(fixture.componentInstance.content().body_markdown).toBe('# Hi');
    expect(fixture.componentInstance.hasUnsavedChanges()).toBe(false);
    fixture.componentInstance.content.update((content) => ({
      ...content,
      body_markdown: '# Updated',
      track_clicks: false,
    }));
    expect(fixture.componentInstance.hasUnsavedChanges()).toBe(true);
    fixture.componentInstance.save();
    const request = http.expectOne(TEMPLATE_URL);
    expect(request.request.method).toBe('PATCH');
    expect(request.request.body).toMatchObject({
      body_markdown: '# Updated',
      track_clicks: false,
    });
    request.flush({ ...template, body_markdown: '# Updated', track_clicks: false });
    expect(fixture.componentInstance.hasUnsavedChanges()).toBe(false);
  });
});
