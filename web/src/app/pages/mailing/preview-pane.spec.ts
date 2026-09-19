import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { MailingPreviewPaneComponent } from './preview-pane';

const BUSINESS_ID = '11111111-1111-4111-8111-111111111111';
const BASE = `/api/v1/businesses/${BUSINESS_ID}/mailing`;

describe('MailingPreviewPaneComponent', () => {
  let http: HttpTestingController;

  beforeEach(() => {
    vi.useFakeTimers();
    TestBed.configureTestingModule({
      providers: [provideHttpClient(), provideHttpClientTesting()],
    });
    http = TestBed.inject(HttpTestingController);
  });

  afterEach(() => {
    http.verify();
    vi.useRealTimers();
  });

  it('debounces 400 ms and ignores a stale preview response', () => {
    const fixture = TestBed.createComponent(MailingPreviewPaneComponent);
    fixture.componentRef.setInput('businessId', BUSINESS_ID);
    fixture.componentRef.setInput('kind', 'campaigns');
    fixture.componentRef.setInput('content', {
      subject: 'One',
      preheader: 'Before',
      body_markdown: '# First',
      track_opens: true,
      track_clicks: true,
    });
    fixture.componentRef.setInput('fromName', 'Acme');
    fixture.detectChanges();
    vi.advanceTimersByTime(399);
    http.expectNone(`${BASE}/campaigns/preview`);
    vi.advanceTimersByTime(1);
    const first = http.expectOne(`${BASE}/campaigns/preview`);

    fixture.componentRef.setInput('content', {
      ...fixture.componentInstance.content,
      body_markdown: '# Second',
    });
    fixture.detectChanges();
    vi.advanceTimersByTime(400);
    const second = http.expectOne(`${BASE}/campaigns/preview`);
    expect(second.request.body).toMatchObject({
      body_markdown: '# Second',
      preheader: 'Before',
      from_name: 'Acme',
    });

    second.flush({ html: '<style>p{color:red}</style><p>Second</p>', text: 'Second' });
    first.flush({ html: '<p>First</p>', text: 'First' });
    fixture.detectChanges();
    expect(fixture.componentInstance.preview().text).toBe('Second');
    const frame = fixture.nativeElement.querySelector('iframe') as HTMLIFrameElement;
    expect(frame.srcdoc).toContain('<style>');
  });

  it('uses the template preview endpoint and renders plain text mode', () => {
    const fixture = TestBed.createComponent(MailingPreviewPaneComponent);
    fixture.componentRef.setInput('businessId', BUSINESS_ID);
    fixture.componentRef.setInput('kind', 'templates');
    fixture.componentRef.setInput('content', {
      subject: '',
      preheader: '',
      body_markdown: 'Hi',
      track_opens: true,
      track_clicks: true,
    });
    fixture.detectChanges();
    vi.advanceTimersByTime(400);
    http.expectOne(`${BASE}/templates/preview`).flush({
      html: '<p>Hi</p>',
      text: 'Hi',
    });
    fixture.componentInstance.mode.set('text');
    fixture.detectChanges();
    expect(fixture.nativeElement.querySelector('pre').textContent).toContain('Hi');
    fixture.destroy();
  });

  it('forwards an unsaved brand override and omits it when absent', () => {
    const fixture = TestBed.createComponent(MailingPreviewPaneComponent);
    fixture.componentRef.setInput('businessId', BUSINESS_ID);
    fixture.componentRef.setInput('kind', 'templates');
    fixture.componentRef.setInput('content', {
      subject: '',
      preheader: '',
      body_markdown: 'Hi',
      track_opens: true,
      track_clicks: true,
    });
    fixture.detectChanges();
    vi.advanceTimersByTime(400);
    const plain = http.expectOne(`${BASE}/templates/preview`);
    expect('brand' in plain.request.body).toBe(false);
    plain.flush({ html: '', text: '' });

    const brand = {
      name: 'Acme',
      colors: {
        background: '#f4f6f8',
        surface: '#ffffff',
        text: '#17212b',
        accent: '#c9501d',
        header_background: '#ffffff',
        header_text: '#17212b',
      },
      font_stack: 'serif',
      footer_markdown: 'Thanks',
    };
    fixture.componentRef.setInput('brand', brand);
    fixture.detectChanges();
    vi.advanceTimersByTime(400);
    const branded = http.expectOne(`${BASE}/templates/preview`);
    expect(branded.request.body.brand).toEqual(brand);
    branded.flush({ html: '', text: '' });
    fixture.destroy();
  });
});
