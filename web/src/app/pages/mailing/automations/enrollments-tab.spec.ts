import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { Automation, Enrollment } from '../../../core/automations.service';
import { MailingSubscriber } from '../../../core/mailing.service';
import { EnrollmentsTabComponent } from './enrollments-tab';

const LIST_ID = '11111111-1111-4111-8111-111111111111';
const BUSINESS_ID = '22222222-2222-4222-8222-222222222222';
const AUTOMATION_ID = '33333333-3333-4333-8333-333333333333';
const VERSION_ID = '44444444-4444-4444-8444-444444444444';
const ENROLLMENT_ID = '55555555-5555-4555-8555-555555555555';
const SECOND_ENROLLMENT_ID = '66666666-6666-4666-8666-666666666666';
const CREATED_ENROLLMENT_ID = '77777777-7777-4777-8777-777777777777';

const activeAutomation: Automation = {
  id: AUTOMATION_ID, business_id: BUSINESS_ID, tenant_root_id: BUSINESS_ID, name: 'Welcome', description: null,
  status: 'active', allow_reenroll: false, active_version_id: VERSION_ID, draft_version_id: null,
  created_by_principal_id: 'u1', created_at: '', updated_at: '',
};

function makeSubscriber(id: string, email: string): MailingSubscriber {
  return {
    id, business_id: BUSINESS_ID, tenant_root_id: BUSINESS_ID, list_id: LIST_ID, email,
    first_name: null, last_name: null, attributes: {}, status: 'active', contact_id: null,
    consent_source: 'manual', consent_attested_by: 'u1', consent_at: '', confirmed_at: null,
    unsubscribed_at: null, status_reason: null, tags: [], created_at: '', updated_at: '',
  };
}

function makeEnrollment(id: string, subscriberId: string, status: Enrollment['status']): Enrollment {
  return {
    id, business_id: BUSINESS_ID, tenant_root_id: BUSINESS_ID, automation_id: AUTOMATION_ID, version_id: VERSION_ID,
    subscriber_id: subscriberId, status,
    current_node_id: status === 'active' ? 'n_welcome' : null,
    wake_at: null, node_attempts: 0,
    last_error: status === 'errored' ? 'template send failed' : null,
    exit_reason: status === 'exited' ? 'manual' : null,
    source_event_id: null, enrolled_at: '2026-09-01T00:00:00Z',
    finished_at: status === 'active' ? null : '2026-09-02T00:00:00Z', updated_at: '',
  };
}

describe('EnrollmentsTabComponent', () => {
  let http: HttpTestingController;
  let fixture: ComponentFixture<EnrollmentsTabComponent>;

  beforeEach(() => {
    TestBed.configureTestingModule({
      providers: [provideHttpClient(), provideHttpClientTesting()],
    });
    http = TestBed.inject(HttpTestingController);
    fixture = TestBed.createComponent(EnrollmentsTabComponent);
    fixture.componentInstance.businessId = BUSINESS_ID;
    fixture.componentInstance.automationId = AUTOMATION_ID;
  });

  function mount(automation: Automation, triggerListId: string | null): void {
    fixture.componentInstance.automation = automation;
    fixture.componentInstance.triggerListId = triggerListId;
    fixture.detectChanges();
  }

  function flushInitial(items: Enrollment[] = []): void {
    http.expectOne((req) => req.url.includes('/enrollments')).flush({ items, next_cursor: null });
    fixture.detectChanges();
  }

  afterEach(() => {
    vi.useRealTimers();
    fixture.destroy();
    http.verify();
    vi.unstubAllGlobals();
  });

  it('lists enrollments with subscriber email, status pill, and exit action for active rows', () => {
    mount(activeAutomation, LIST_ID);
    const target = makeEnrollment(ENROLLMENT_ID, '88888888-8888-4888-8888-888888888888', 'active');
    flushInitial([target]);
    http.expectOne((req) => req.url.includes('/subscribers')).flush({
      items: [makeSubscriber(target.subscriber_id, 'ada@acme.test')],
      next_cursor: null,
    });
    fixture.detectChanges();

    expect(fixture.nativeElement.querySelector('[data-testid="enrollment-row-subscriber"]').textContent).toContain('ada@acme.test');
    expect(fixture.nativeElement.querySelector('[data-testid="enrollments-table"]')?.getAttribute('role')).toBe('table');
    expect(fixture.nativeElement.querySelector('[data-testid="enrollment-row"] .mf-pill')?.textContent).toContain('active');
    expect(fixture.nativeElement.querySelector('[data-testid="enrollment-exit"]')).toBeTruthy();
    expect(fixture.nativeElement.querySelector('[data-testid="enrollments-empty"]')).toBeNull();
  });

  it('shows the empty state when there are no enrollments', () => {
    mount(activeAutomation, LIST_ID);
    flushInitial([]);
    expect(fixture.nativeElement.querySelector('[data-testid="enrollments-empty"]')).toBeTruthy();
  });

  it('sends the status filter on the reload request', () => {
    mount(activeAutomation, LIST_ID);
    flushInitial([]);
    fixture.componentInstance.setStatusFilter('exited');
    const request = http.expectOne((req) => req.url.includes('/enrollments'));
    expect(request.request.params.get('status')).toBe('exited');
    request.flush({ items: [], next_cursor: null });
    fixture.detectChanges();
  });

  it('disables the enroll button when the automation is not active', () => {
    mount({ ...activeAutomation, status: 'paused' }, LIST_ID);
    flushInitial([]);
    expect((fixture.nativeElement.querySelector('[data-testid="enrollment-enroll"]') as HTMLButtonElement).disabled).toBe(true);
  });

  it('disables the enroll button when no trigger list is available', () => {
    mount(activeAutomation, null);
    flushInitial([]);
    expect((fixture.nativeElement.querySelector('[data-testid="enrollment-enroll"]') as HTMLButtonElement).disabled).toBe(true);
  });

  it('enrolls the chosen candidate from the search dialog and reloads', () => {
    vi.useFakeTimers();
    mount(activeAutomation, LIST_ID);
    flushInitial([]);

    (fixture.nativeElement.querySelector('[data-testid="enrollment-enroll"]') as HTMLButtonElement).click();
    fixture.detectChanges();
    expect(fixture.nativeElement.querySelector('[data-testid="enroll-dialog-backdrop"]')).toBeTruthy();

    const candidate = makeSubscriber('99999999-9999-4999-8999-999999999999', 'grace@acme.test');
    const input = fixture.nativeElement.querySelector('[data-testid="enroll-search"]') as HTMLInputElement;
    input.value = 'grace';
    input.dispatchEvent(new Event('input'));
    vi.advanceTimersByTime(300);
    fixture.detectChanges();
    const search = http.expectOne((req) => req.url.includes('/subscribers'));
    expect(search.request.params.get('q')).toBe('grace');
    search.flush({ items: [candidate], next_cursor: null });
    fixture.detectChanges();
    expect(fixture.nativeElement.querySelectorAll('[data-testid="enroll-candidate"]')).toHaveLength(1);

    (fixture.nativeElement.querySelector('[data-testid="enroll-candidate-select"]') as HTMLButtonElement).click();
    fixture.detectChanges();
    const enrollment = makeEnrollment(CREATED_ENROLLMENT_ID, candidate.id, 'active');
    const enroll = http.expectOne((req) => req.url.includes('/enrollments') && req.method === 'POST');
    expect(enroll.request.body).toEqual({ subscriber_id: candidate.id });
    enroll.flush(enrollment, { status: 201, statusText: 'Created' });
    fixture.detectChanges();

    const reload = http.expectOne((req) => req.url.includes('/enrollments') && req.method === 'GET');
    reload.flush({ items: [enrollment], next_cursor: null });
    http.expectOne((req) => req.url.includes('/subscribers')).flush({ items: [candidate], next_cursor: null });
    fixture.detectChanges();
    expect(fixture.nativeElement.querySelector('[data-testid="enrollments-table"]')?.textContent).toContain('grace@acme.test');
    expect(fixture.nativeElement.querySelector('[data-testid="enroll-dialog-backdrop"]')).toBeNull();
  });

  it('reloads without crashing when enrollment returns a conflict', () => {
    vi.useFakeTimers();
    mount(activeAutomation, LIST_ID);
    flushInitial([]);
    (fixture.nativeElement.querySelector('[data-testid="enrollment-enroll"]') as HTMLButtonElement).click();
    fixture.detectChanges();
    const candidate = makeSubscriber('aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa', 'bob@acme.test');
    const input = fixture.nativeElement.querySelector('[data-testid="enroll-search"]') as HTMLInputElement;
    input.value = 'bob';
    input.dispatchEvent(new Event('input'));
    vi.advanceTimersByTime(300);
    fixture.detectChanges();
    http.expectOne((req) => req.url.includes('/subscribers')).flush({ items: [candidate], next_cursor: null });
    fixture.detectChanges();

    (fixture.nativeElement.querySelector('[data-testid="enroll-candidate-select"]') as HTMLButtonElement).click();
    fixture.detectChanges();

    http
      .expectOne((req) => req.url.includes('/enrollments') && req.method === 'POST')
      .flush({ code: 'CONFLICT', message: 'already enrolled' }, { status: 409, statusText: 'Conflict' });
    fixture.detectChanges();
    http.expectOne((req) => req.url.includes('/enrollments') && req.method === 'GET').flush({ items: [], next_cursor: null });
    fixture.detectChanges();
    expect(fixture.nativeElement.querySelector('[data-testid="enrollments-table"]')).toBeTruthy();
  });

  it('exits an active enrollment after confirmation and reloads', () => {
    vi.stubGlobal('confirm', vi.fn().mockReturnValue(true));
    mount(activeAutomation, LIST_ID);
    const target = makeEnrollment(ENROLLMENT_ID, 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb', 'active');
    flushInitial([target]);
    http.expectOne((req) => req.url.includes('/subscribers')).flush({ items: [], next_cursor: null });
    fixture.detectChanges();

    (fixture.nativeElement.querySelector('[data-testid="enrollment-exit"]') as HTMLButtonElement).click();
    const exit = http.expectOne((req) => req.url.includes('/exit'));
    expect(exit.request.url).toContain(`/enrollments/${ENROLLMENT_ID}/exit`);
    exit.flush({ ...target, status: 'exited', exit_reason: 'manual', current_node_id: null });
    fixture.detectChanges();

    const reload = http.expectOne((req) => req.url.includes('/enrollments') && req.method === 'GET');
    reload.flush({ items: [{ ...target, status: 'exited' as const, exit_reason: 'manual' }], next_cursor: null });
    http.expectOne((req) => req.url.includes('/subscribers')).flush({ items: [], next_cursor: null });
    fixture.detectChanges();
    expect(fixture.nativeElement.querySelector('[data-testid="enrollment-exit"]')).toBeNull();
    expect(fixture.nativeElement.querySelector('[data-testid="enrollments-table"]')?.textContent).toContain('manual');
  });

  it('does not call exit when confirmation is declined', () => {
    vi.stubGlobal('confirm', vi.fn().mockReturnValue(false));
    mount(activeAutomation, LIST_ID);
    flushInitial([makeEnrollment(ENROLLMENT_ID, 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb', 'active')]);
    http.expectOne((req) => req.url.includes('/subscribers')).flush({ items: [], next_cursor: null });
    fixture.detectChanges();
    (fixture.nativeElement.querySelector('[data-testid="enrollment-exit"]') as HTMLButtonElement).click();
    fixture.detectChanges();
    http.expectNone((req) => req.url.includes('/exit'));
  });

  it('appends the next page when Load more is clicked', () => {
    mount(activeAutomation, LIST_ID);
    const first = makeEnrollment(ENROLLMENT_ID, '88888888-8888-4888-8888-888888888888', 'active');
    const second = makeEnrollment(SECOND_ENROLLMENT_ID, '99999999-9999-4999-8999-999999999999', 'active');
    http.expectOne((req) => req.url.includes('/enrollments')).flush({ items: [first], next_cursor: 'c1' });
    fixture.detectChanges();
    http.expectOne((req) => req.url.includes('/subscribers')).flush({
      items: [makeSubscriber(first.subscriber_id, 'ada@acme.test')], next_cursor: null,
    });
    fixture.detectChanges();

    (fixture.nativeElement.querySelector('[data-testid="enrollments-load-more"]') as HTMLButtonElement).click();
    const more = http.expectOne((req) => req.url.includes('/enrollments'));
    expect(more.request.params.get('cursor')).toBe('c1');
    more.flush({ items: [second], next_cursor: null });
    fixture.detectChanges();
    http.expectOne((req) => req.url.includes('/subscribers')).flush({
      items: [makeSubscriber(second.subscriber_id, 'grace@acme.test')], next_cursor: null,
    });
    fixture.detectChanges();

    const rows = fixture.nativeElement.querySelectorAll('[data-testid="enrollment-row"]');
    expect(rows).toHaveLength(2);
    expect(fixture.nativeElement.querySelector('[data-testid="enrollments-load-more"]')).toBeNull();
  });

  it('shows an error state when the initial load fails', () => {
    mount(activeAutomation, LIST_ID);
    http.expectOne((req) => req.url.includes('/enrollments')).error(new ErrorEvent('boom'));
    fixture.detectChanges();
    expect(fixture.nativeElement.querySelector('[data-testid="enrollments-error"]')).toBeTruthy();
  });

  it('closes the enroll dialog with Escape', () => {
    mount(activeAutomation, LIST_ID);
    flushInitial([]);
    (fixture.nativeElement.querySelector('[data-testid="enrollment-enroll"]') as HTMLButtonElement).click();
    fixture.detectChanges();
    expect(fixture.nativeElement.querySelector('[data-testid="enroll-dialog-backdrop"]')).toBeTruthy();

    const backdrop = fixture.nativeElement.querySelector('[data-testid="enroll-dialog-backdrop"]') as HTMLElement;
    backdrop.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
    fixture.detectChanges();
    expect(fixture.nativeElement.querySelector('[data-testid="enroll-dialog-backdrop"]')).toBeNull();
  });
});
