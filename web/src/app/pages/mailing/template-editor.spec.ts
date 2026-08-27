import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { TestBed } from '@angular/core/testing';
import { ActivatedRoute, convertToParamMap, provideRouter } from '@angular/router';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { MailingTemplateEditorComponent } from './template-editor';

const template = {
  id: 't1',
  business_id: 'b1',
  tenant_root_id: 'b1',
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
            snapshot: { paramMap: convertToParamMap({ businessId: 'b1', templateId: 't1' }) },
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
    http.expectOne('/api/v1/businesses/b1/mailing/templates/t1').flush(template);
    fixture.detectChanges();
    expect(fixture.componentInstance.bodyMarkdown).toBe('# Hi');
    fixture.componentInstance.bodyMarkdown = '# Updated';
    fixture.componentInstance.trackClicks = false;
    fixture.componentInstance.save();
    const request = http.expectOne('/api/v1/businesses/b1/mailing/templates/t1');
    expect(request.request.method).toBe('PATCH');
    expect(request.request.body).toMatchObject({
      body_markdown: '# Updated',
      track_clicks: false,
    });
    request.flush({ ...template, body_markdown: '# Updated', track_clicks: false });
  });
});
