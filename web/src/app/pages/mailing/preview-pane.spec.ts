import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { MailingPreviewPaneComponent } from './preview-pane';

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
    fixture.componentRef.setInput('businessId', 'b1');
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
    http.expectNone('/api/v1/businesses/b1/mailing/campaigns/preview');
    vi.advanceTimersByTime(1);
    const first = http.expectOne('/api/v1/businesses/b1/mailing/campaigns/preview');

    fixture.componentRef.setInput('content', {
      ...fixture.componentInstance.content,
      body_markdown: '# Second',
    });
    fixture.detectChanges();
    vi.advanceTimersByTime(400);
    const second = http.expectOne('/api/v1/businesses/b1/mailing/campaigns/preview');
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
    fixture.componentRef.setInput('businessId', 'b1');
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
    http.expectOne('/api/v1/businesses/b1/mailing/templates/preview').flush({
      html: '<p>Hi</p>',
      text: 'Hi',
    });
    fixture.componentInstance.mode.set('text');
    fixture.detectChanges();
    expect(fixture.nativeElement.querySelector('pre').textContent).toContain('Hi');
    fixture.destroy();
  });
});
