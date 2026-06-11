import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { ActivatedRoute, provideRouter } from '@angular/router';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { AssignableMember, Page, Ticket, TicketMessage } from '../../core/ticket.service';
import { ThreadViewComponent } from './thread-view';

// Component-level coverage for the US3 triage controls. We drive the real
// component against a mock backend (HttpTestingController) so the wiring —
// status/priority/tags/assignee → patchTicket → reflected Ticket — is pinned,
// following the service spec's mock-backend style. The route supplies the
// business + ticket ids the component reads in ngOnInit.
const biz = 'b1';
const tid = 't1';
const myPid = 'principal-self';

function makeTicket(over: Partial<Ticket> = {}): Ticket {
  return {
    id: tid,
    business_id: biz,
    tenant_root_id: 'root',
    subject: 'Help',
    status: 'open',
    priority: 'normal',
    assignee_principal_id: null,
    requester: {
      id: 'r1',
      tenant_root_id: 'root',
      email: 'a@b.test',
      display_name: null,
      contact_id: null,
      first_seen_at: '2024-01-01T00:00:00Z',
      last_seen_at: '2024-01-01T00:00:00Z',
    },
    tags: [],
    message_count: 0,
    last_message_at: null,
    created_at: '2024-01-01T00:00:00Z',
    updated_at: '2024-01-01T00:00:00Z',
    ...over,
  };
}

const emptyPage: Page<TicketMessage> = { items: [], next_cursor: null };

describe('ThreadViewComponent triage (US3)', () => {
  let fixture: ComponentFixture<ThreadViewComponent>;
  let cmp: ThreadViewComponent;
  let mock: HttpTestingController;

  // Bring the component to a loaded state: flush /me, the ticket, and its
  // (empty) message thread, leaving the triage controls live.
  function loadWith(t: Ticket, members: AssignableMember[] = []): void {
    fixture = TestBed.createComponent(ThreadViewComponent);
    cmp = fixture.componentInstance;
    fixture.detectChanges(); // ngOnInit fires /me + assignable-members + getTicket

    mock.expectOne('/api/v1/me').flush({
      id: myPid,
      email: 'me@x.test',
      display_name: 'Me',
      email_verified: true,
      status: 'active',
    });
    mock
      .expectOne(`/api/v1/businesses/${biz}/assignable-members`)
      .flush({ items: members, next_cursor: null });
    mock.expectOne(`/api/v1/businesses/${biz}/tickets/${tid}`).flush(t);
    mock
      .expectOne((r) => r.url === `/api/v1/businesses/${biz}/tickets/${tid}/messages`)
      .flush(emptyPage);
    fixture.detectChanges();
  }

  beforeEach(() => {
    TestBed.configureTestingModule({
      providers: [
        provideHttpClient(),
        provideHttpClientTesting(),
        provideRouter([]),
        {
          provide: ActivatedRoute,
          useValue: {
            snapshot: {
              paramMap: new Map([
                ['businessId', biz],
                ['tid', tid],
              ]),
            },
          },
        },
      ],
    });
    mock = TestBed.inject(HttpTestingController);
  });

  afterEach(() => mock.verify());

  it('changing status PATCHes only {status} and reflects the returned ticket', () => {
    loadWith(makeTicket({ status: 'open' }));

    cmp.changeStatus('solved');
    const req = mock.expectOne(`/api/v1/businesses/${biz}/tickets/${tid}`);
    expect(req.request.method).toBe('PATCH');
    expect(req.request.body).toEqual({ status: 'solved' });
    expect('assignee_principal_id' in (req.request.body as object)).toBe(false);

    req.flush(makeTicket({ status: 'solved' }));
    expect(cmp.ticket()!.status).toBe('solved');
  });

  it('changing status to the same value is a no-op (no request)', () => {
    loadWith(makeTicket({ status: 'open' }));
    cmp.changeStatus('open');
    mock.expectNone(`/api/v1/businesses/${biz}/tickets/${tid}`);
  });

  it('changing priority PATCHes only {priority}', () => {
    loadWith(makeTicket({ priority: 'normal' }));
    cmp.changePriority('urgent');
    const req = mock.expectOne(`/api/v1/businesses/${biz}/tickets/${tid}`);
    expect(req.request.body).toEqual({ priority: 'urgent' });
    req.flush(makeTicket({ priority: 'urgent' }));
    expect(cmp.ticket()!.priority).toBe('urgent');
  });

  it('adding a tag sends the FULL resulting set', () => {
    loadWith(makeTicket({ tags: ['billing'] }));
    cmp.tagDraft = 'vip';
    cmp.addTag();
    const req = mock.expectOne(`/api/v1/businesses/${biz}/tickets/${tid}`);
    expect(req.request.body).toEqual({ tags: ['billing', 'vip'] });
    req.flush(makeTicket({ tags: ['billing', 'vip'] }));
    expect(cmp.ticket()!.tags).toEqual(['billing', 'vip']);
    expect(cmp.tagDraft).toBe(''); // cleared on success
  });

  it('removing a tag sends the full remaining set', () => {
    loadWith(makeTicket({ tags: ['billing', 'vip'] }));
    cmp.removeTag('billing');
    const req = mock.expectOne(`/api/v1/businesses/${biz}/tickets/${tid}`);
    expect(req.request.body).toEqual({ tags: ['vip'] });
    req.flush(makeTicket({ tags: ['vip'] }));
    expect(cmp.ticket()!.tags).toEqual(['vip']);
  });

  it('a duplicate tag (case-insensitive) is not sent', () => {
    loadWith(makeTicket({ tags: ['Billing'] }));
    cmp.tagDraft = 'billing';
    cmp.addTag();
    mock.expectNone(`/api/v1/businesses/${biz}/tickets/${tid}`);
    expect(cmp.tagDraft).toBe('');
  });

  it('assign-to-me PATCHes the caller principal id from /me', () => {
    loadWith(makeTicket({ assignee_principal_id: null }));
    expect(cmp.myPrincipalId()).toBe(myPid);

    cmp.assignToMe();
    const req = mock.expectOne(`/api/v1/businesses/${biz}/tickets/${tid}`);
    expect(req.request.body).toEqual({ assignee_principal_id: myPid });
    req.flush(makeTicket({ assignee_principal_id: myPid }));
    expect(cmp.ticket()!.assignee_principal_id).toBe(myPid);
  });

  it('unassign sends literal null', () => {
    loadWith(makeTicket({ assignee_principal_id: myPid }));
    cmp.unassign();
    const req = mock.expectOne(`/api/v1/businesses/${biz}/tickets/${tid}`);
    expect(req.request.body).toEqual({ assignee_principal_id: null });
    expect('assignee_principal_id' in (req.request.body as object)).toBe(true);
    expect(
      (req.request.body as { assignee_principal_id: string | null }).assignee_principal_id,
    ).toBeNull();
    req.flush(makeTicket({ assignee_principal_id: null }));
    expect(cmp.ticket()!.assignee_principal_id).toBeNull();
  });

  it('manual assign sends the entered uuid and clears the draft', () => {
    loadWith(makeTicket({ assignee_principal_id: null }));
    cmp.assigneeDraft = 'other-principal';
    cmp.assignManual();
    const req = mock.expectOne(`/api/v1/businesses/${biz}/tickets/${tid}`);
    expect(req.request.body).toEqual({ assignee_principal_id: 'other-principal' });
    req.flush(makeTicket({ assignee_principal_id: 'other-principal' }));
    expect(cmp.assigneeDraft).toBe('');
  });

  it('surfaces a 409 (ineligible assignee) without crashing and keeps the ticket', () => {
    loadWith(makeTicket({ assignee_principal_id: null }));
    cmp.assigneeDraft = 'bad-principal';
    cmp.assignManual();
    const req = mock.expectOne(`/api/v1/businesses/${biz}/tickets/${tid}`);
    req.flush({ code: 'CONFLICT', message: 'ineligible' }, { status: 409, statusText: 'Conflict' });
    expect(cmp.triageError()).toContain('conflicts');
    expect(cmp.ticket()!.assignee_principal_id).toBeNull(); // unchanged
  });

  it('renders the assignee picker from listAssignableMembers (Unassigned + members)', () => {
    const members: AssignableMember[] = [
      { id: 'p-alice', email: 'alice@x.test', display_name: 'Alice' },
      { id: 'p-bob', email: 'bob@x.test', display_name: 'Bob' },
    ];
    loadWith(makeTicket({ assignee_principal_id: null }), members);
    expect(cmp.members().length).toBe(2);
    const picker = fixture.nativeElement.querySelector(
      '[data-testid="assignee-picker"]',
    ) as HTMLSelectElement;
    expect(picker).toBeTruthy();
    // One "Unassigned" option plus one per member.
    expect(picker.querySelectorAll('option').length).toBe(3);
  });

  it('picking a member PATCHes its principal id; reselecting the current is a no-op', () => {
    const members: AssignableMember[] = [{ id: 'p-bob', email: 'bob@x.test', display_name: 'Bob' }];
    loadWith(makeTicket({ assignee_principal_id: null }), members);

    cmp.assignPicked('p-bob');
    const req = mock.expectOne(`/api/v1/businesses/${biz}/tickets/${tid}`);
    expect(req.request.body).toEqual({ assignee_principal_id: 'p-bob' });
    req.flush(makeTicket({ assignee_principal_id: 'p-bob' }));
    expect(cmp.ticket()!.assignee_principal_id).toBe('p-bob');

    // Reselecting the now-current assignee fires no redundant PATCH.
    cmp.assignPicked('p-bob');
    mock.expectNone(`/api/v1/businesses/${biz}/tickets/${tid}`);
  });

  it('picking (unassigned) PATCHes literal null', () => {
    const members: AssignableMember[] = [{ id: 'p-bob', email: 'bob@x.test', display_name: 'Bob' }];
    loadWith(makeTicket({ assignee_principal_id: 'p-bob' }), members);

    cmp.assignPicked('');
    const req = mock.expectOne(`/api/v1/businesses/${biz}/tickets/${tid}`);
    expect(req.request.body).toEqual({ assignee_principal_id: null });
    expect('assignee_principal_id' in (req.request.body as object)).toBe(true);
    req.flush(makeTicket({ assignee_principal_id: null }));
    expect(cmp.ticket()!.assignee_principal_id).toBeNull();
  });

  it('hides the picker when no assignable members load (404/empty)', () => {
    loadWith(makeTicket({ assignee_principal_id: null })); // members default to []
    const picker = fixture.nativeElement.querySelector('[data-testid="assignee-picker"]');
    expect(picker).toBeNull();
  });

  it('surfaces a 400 validation message (empty/whitespace tag) gracefully', () => {
    loadWith(makeTicket({ tags: [] }));
    cmp.tagDraft = 'x';
    cmp.addTag();
    const req = mock.expectOne(`/api/v1/businesses/${biz}/tickets/${tid}`);
    req.flush(
      { code: 'VALIDATION', message: 'tag must not be blank' },
      { status: 400, statusText: 'Bad Request' },
    );
    expect(cmp.triageError()).toContain('tag must not be blank');
  });

  it('maps a 404 (unknown OR missing tickets.assign) to a no-oracle message', () => {
    loadWith(makeTicket({ assignee_principal_id: null }));
    cmp.assigneeDraft = 'someone';
    cmp.assignManual();
    const req = mock.expectOne(`/api/v1/businesses/${biz}/tickets/${tid}`);
    req.flush(
      { code: 'NOT_FOUND', message: 'not found' },
      { status: 404, statusText: 'Not Found' },
    );
    expect(cmp.triageError()).toBe("You don't have access to do that.");
  });
});

// Task 20 UI-redesign render coverage. Drives the real component against a mock
// backend and asserts the design-system markup (mf-card, mf-select, mf-textarea,
// mf-btn, mf-status-pill) is present and the data-testid contract is preserved,
// in both light and dark themes. Mirrors ticket-list.spec.ts style.
function makeMessage(over: Partial<TicketMessage> = {}): TicketMessage {
  return {
    id: 'm1',
    ticket_id: tid,
    direction: 'inbound',
    body_text: 'Hello there',
    delivery_state: 'delivered',
    spf_result: 'pass',
    dkim_result: 'pass',
    dmarc_result: 'pass',
    attachments: [],
    created_at: '2024-01-01T00:00:00Z',
    ...over,
  } as TicketMessage;
}

describe('ThreadViewComponent (Task 20 UI redesign)', () => {
  let fixture: ComponentFixture<ThreadViewComponent>;
  let mock: HttpTestingController;

  function boot(
    t: Ticket = makeTicketTv(),
    msgs: TicketMessage[] = [makeMessage()],
    members: AssignableMember[] = [],
  ): void {
    fixture = TestBed.createComponent(ThreadViewComponent);
    fixture.detectChanges(); // ngOnInit → /me + assignable-members + getTicket
    mock.expectOne('/api/v1/me').flush({
      id: myPid,
      email: 'me@x.test',
      display_name: 'Me',
      email_verified: true,
      status: 'active',
    });
    mock
      .expectOne(`/api/v1/businesses/${biz}/assignable-members`)
      .flush({ items: members, next_cursor: null });
    mock.expectOne(`/api/v1/businesses/${biz}/tickets/${tid}`).flush(t);
    mock
      .expectOne((r) => r.url === `/api/v1/businesses/${biz}/tickets/${tid}/messages`)
      .flush({ items: msgs, next_cursor: null });
    fixture.detectChanges();
  }

  function q(sel: string): HTMLElement | null {
    return fixture.nativeElement.querySelector(sel) as HTMLElement | null;
  }

  beforeEach(() => {
    TestBed.configureTestingModule({
      providers: [
        provideHttpClient(),
        provideHttpClientTesting(),
        provideRouter([]),
        {
          provide: ActivatedRoute,
          useValue: {
            snapshot: {
              paramMap: new Map([
                ['businessId', biz],
                ['tid', tid],
              ]),
            },
          },
        },
      ],
    });
    mock = TestBed.inject(HttpTestingController);
    document.documentElement.setAttribute('data-theme', 'light');
  });

  afterEach(() => {
    mock.verify();
    document.documentElement.setAttribute('data-theme', 'light');
  });

  it('renders the thread subject', () => {
    boot(makeTicketTv({ subject: 'Printer down' }));
    const subj = q('[data-testid="thread-subject"]');
    expect(subj).not.toBeNull();
    expect(subj!.textContent).toContain('Printer down');
  });

  it('triage-status is a select.mf-select', () => {
    boot();
    const sel = q('[data-testid="triage-status"]');
    expect(sel).not.toBeNull();
    expect(sel!.tagName.toLowerCase()).toBe('select');
    expect(sel!.classList.contains('mf-select')).toBe(true);
  });

  it('composer-body is a textarea.mf-textarea', () => {
    boot();
    const ta = q('[data-testid="composer-body"]');
    expect(ta).not.toBeNull();
    expect(ta!.tagName.toLowerCase()).toBe('textarea');
    expect(ta!.classList.contains('mf-textarea')).toBe(true);
  });

  it('composer-submit has the mf-btn class', () => {
    boot();
    const btn = q('[data-testid="composer-submit"]');
    expect(btn).not.toBeNull();
    expect(btn!.classList.contains('mf-btn')).toBe(true);
  });

  it('preserves the header, message-thread, and composer testids', () => {
    boot();
    for (const id of [
      'back-to-list',
      'thread-header',
      'thread-status',
      'thread-priority',
      'thread-requester',
      'triage',
      'triage-priority',
      'triage-tags',
      'triage-tag-input',
      'triage-assignee',
      'assign-to-me',
      'unassign',
      'assign-uuid-input',
      'assign-uuid-submit',
      'message-thread',
      'message',
      'message-direction',
      'message-body',
      'composer',
      'composer-toggle',
      'toggle-reply',
      'toggle-note',
    ]) {
      expect(q(`[data-testid="${id}"]`)).not.toBeNull();
    }
  });

  it('renders inbound auth flags (spf/dkim/dmarc) for inbound messages', () => {
    boot(makeTicketTv(), [makeMessage({ direction: 'inbound' })]);
    expect(q('[data-testid="auth-flags"]')).not.toBeNull();
    expect(q('[data-testid="spf-result"]')).not.toBeNull();
    expect(q('[data-testid="dkim-result"]')).not.toBeNull();
    expect(q('[data-testid="dmarc-result"]')).not.toBeNull();
  });

  it('dark-theme: .mf-card is present', () => {
    document.documentElement.setAttribute('data-theme', 'dark');
    boot();
    expect(q('.mf-card')).not.toBeNull();
  });
});

function makeTicketTv(over: Partial<Ticket> = {}): Ticket {
  return makeTicket(over);
}
