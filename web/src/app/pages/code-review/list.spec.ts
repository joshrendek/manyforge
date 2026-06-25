import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { provideRouter } from '@angular/router';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { ToastService } from '../../ui/toast/toast.service';
import { CodeReviewListComponent } from './list';

const biz = {
  items: [{ id: 'b1', parent_id: null, tenant_root_id: 'b1', name: 'Acme', status: 'active', is_tenant_root: true }],
  next_cursor: null,
};

const connectors = {
  items: [
    {
      id: 'c1', type: 'github', display_name: 'acme/api', repo: 'acme/api',
      base_url: 'https://api.github.com', allow_private_base_url: false,
      status: 'active', created_at: '2026-06-01T00:00:00Z',
    },
  ],
};

const agentsResp = {
  items: [
    {
      id: 'ag1', business_id: 'b1', principal_id: 'p1', name: 'Reviewer Agent',
      provider: 'anthropic', model: 'claude-opus-4-8',
      system_prompt: '', allowed_tools: [], autonomy_mode: 1, enabled: true,
      monthly_budget_cents: 2500, allowed_mcp_servers: [], retriage_on_reply: false,
      created_at: '', updated_at: '',
    },
  ],
};

const reviews = { items: [] };

describe('CodeReviewListComponent', () => {
  let mock: HttpTestingController;

  beforeEach(() => {
    localStorage.clear();
    TestBed.configureTestingModule({
      providers: [provideHttpClient(), provideHttpClientTesting(), provideRouter([])],
    });
    mock = TestBed.inject(HttpTestingController);
  });

  afterEach(() => {
    document.documentElement.setAttribute('data-theme', 'light');
    localStorage.clear();
  });

  /** Mount the component and flush all three init requests. */
  function mount(): ComponentFixture<CodeReviewListComponent> {
    const f = TestBed.createComponent(CodeReviewListComponent);
    f.detectChanges();
    mock.expectOne('/api/v1/businesses').flush(biz);
    f.detectChanges();
    mock.expectOne('/api/v1/businesses/b1/repo-connectors').flush(connectors);
    mock.expectOne('/api/v1/businesses/b1/code-reviews').flush(reviews);
    mock.expectOne('/api/v1/businesses/b1/agents').flush(agentsResp);
    f.detectChanges();
    return f;
  }

  // ── Connectors list ─────────────────────────────────────────────────────────

  it('loads businesses then lists connectors', () => {
    const f = mount();
    expect(f.componentInstance.connectors().length).toBe(1);
    expect(f.componentInstance.connectors()[0].display_name).toBe('acme/api');
  });

  it('renders a connector row with display name', () => {
    const el: HTMLElement = mount().nativeElement;
    const row = el.querySelector('[data-testid="connector-row"]');
    expect(row).toBeTruthy();
    expect(row?.textContent).toContain('acme/api');
  });

  // ── Add-connector form ───────────────────────────────────────────────────────

  it('add toggle reveals the add form', () => {
    const f = mount();
    const el: HTMLElement = f.nativeElement;
    expect(el.querySelector('[data-testid="connector-add-form"]')).toBeNull();
    (el.querySelector('[data-testid="connector-add-toggle"]') as HTMLButtonElement).click();
    f.detectChanges();
    expect(el.querySelector('[data-testid="connector-add-form"]')).toBeTruthy();
  });

  it('add form submits createConnector and refreshes connector list', () => {
    const f = mount();
    const el: HTMLElement = f.nativeElement;

    // Open the add form.
    (el.querySelector('[data-testid="connector-add-toggle"]') as HTMLButtonElement).click();
    f.detectChanges();

    // Set the ngModel-bound form model directly.
    f.componentInstance.addForm.display_name = 'My Connector';
    f.componentInstance.addForm.repo = 'acme/new';
    f.componentInstance.addForm.api_token = 'ghp_test';

    // Submit.
    (el.querySelector('[data-testid="connector-form-submit"]') as HTMLButtonElement).click();
    f.detectChanges();

    const postReq = mock.expectOne('/api/v1/businesses/b1/repo-connectors');
    expect(postReq.request.method).toBe('POST');
    expect(postReq.request.body.type).toBe('github');
    postReq.flush({ id: 'c2' });
    f.detectChanges();

    // After save the service reloads connectors.
    mock.expectOne('/api/v1/businesses/b1/repo-connectors').flush({
      items: [...connectors.items, {
        id: 'c2', type: 'github', display_name: 'My Connector', repo: 'acme/new',
        base_url: 'https://api.github.com', allow_private_base_url: false,
        status: 'active', created_at: '2026-06-25T00:00:00Z',
      }],
    });
    f.detectChanges();

    expect(f.componentInstance.connectors().length).toBe(2);
  });

  // ── Delete connector ─────────────────────────────────────────────────────────

  it('delete asks for confirm, then DELETEs and removes the row', () => {
    const f = mount();
    const el: HTMLElement = f.nativeElement;

    (el.querySelector('[data-testid="connector-delete"]') as HTMLButtonElement).click();
    f.detectChanges();
    expect(el.querySelector('[data-testid="connector-delete-confirm"]')).toBeTruthy();

    (el.querySelector('[data-testid="connector-delete-yes"]') as HTMLButtonElement).click();
    mock.expectOne({ method: 'DELETE', url: '/api/v1/businesses/b1/repo-connectors/c1' }).flush(null);
    f.detectChanges();

    expect(el.querySelector('[data-testid="connector-row"]')).toBeNull();
    expect(f.componentInstance.connectors().length).toBe(0);
  });

  it('cancelling delete confirm leaves the row in place', () => {
    const f = mount();
    const el: HTMLElement = f.nativeElement;

    (el.querySelector('[data-testid="connector-delete"]') as HTMLButtonElement).click();
    f.detectChanges();
    (el.querySelector('[data-testid="connector-delete-no"]') as HTMLButtonElement).click();
    f.detectChanges();

    expect(el.querySelector('[data-testid="connector-delete-confirm"]')).toBeNull();
    expect(el.querySelector('[data-testid="connector-row"]')).toBeTruthy();
  });

  // ── Review-a-PR trigger ──────────────────────────────────────────────────────

  /** Helper: set trigger form fields and flush the detectChanges cycle cleanly. */
  function setTriggerForm(
    f: ComponentFixture<CodeReviewListComponent>,
    agentId: string,
    connectorId: string,
    prNumber: number,
  ): void {
    // Mutate the plain-object form model then call triggerReview() directly to
    // avoid the NG0100 ExpressionChangedAfterChecked error that occurs when the
    // test sets the values between detectChanges cycles while ngModel is bound.
    f.componentInstance.triggerForm.agent_id = agentId;
    f.componentInstance.triggerForm.repo_connector_id = connectorId;
    f.componentInstance.triggerForm.pr_number = prNumber;
  }

  it('cr-submit is disabled until agent, connector, and PR number are all set', async () => {
    const f = mount();
    const el: HTMLElement = f.nativeElement;
    const submit = () => el.querySelector('[data-testid="cr-submit"]') as HTMLButtonElement;

    // Drive the real form controls (set value + fire the event ngModel listens for)
    // so model<->view stay consistent. Auto change-detection lets the [(ngModel)]
    // write-backs settle asynchronously, avoiding the NG0100 check-no-changes trip.
    f.autoDetectChanges(true);
    const setSelect = async (testid: string, value: string) => {
      const sel = el.querySelector(`[data-testid="${testid}"]`) as HTMLSelectElement;
      sel.value = Array.from(sel.options).find((o) => o.value.includes(value))?.value ?? value;
      sel.dispatchEvent(new Event('change'));
      await f.whenStable();
    };
    const setNumber = async (testid: string, value: string) => {
      const inp = el.querySelector(`[data-testid="${testid}"]`) as HTMLInputElement;
      inp.value = value;
      inp.dispatchEvent(new Event('input'));
      await f.whenStable();
    };

    // Nothing selected → disabled.
    expect(submit().disabled).toBe(true);

    // Only agent → still disabled.
    await setSelect('cr-agent', 'ag1');
    expect(submit().disabled).toBe(true);

    // Agent + connector but no PR number → still disabled.
    await setSelect('cr-connector', 'c1');
    expect(submit().disabled).toBe(true);

    // All three filled → enabled.
    await setNumber('cr-pr-number', '42');
    expect(submit().disabled).toBe(false);
  });

  it('Review-a-PR submit calls trigger with the selected agent, connector, and PR number', () => {
    const f = mount();
    setTriggerForm(f, 'ag1', 'c1', 42);

    // Call triggerReview directly — bypasses the DOM click that would trigger
    // another change-detection cycle on the mutated model.
    f.componentInstance.triggerReview();

    const req = mock.expectOne('/api/v1/businesses/b1/code-reviews');
    expect(req.request.method).toBe('POST');
    expect(req.request.body).toEqual({ agent_id: 'ag1', repo_connector_id: 'c1', pr_number: 42 });
    req.flush({ id: 'r1', status: 'pending', review_url: '' });
    f.detectChanges();

    expect(f.componentInstance.triggerSuccess()).toBe(true);
  });

  it('trigger shows optimistic pending row in reviews after 202', () => {
    const f = mount();
    setTriggerForm(f, 'ag1', 'c1', 7);

    f.componentInstance.triggerReview();
    mock.expectOne('/api/v1/businesses/b1/code-reviews').flush({ id: 'r2', status: 'pending', review_url: '' });
    f.detectChanges();

    expect(f.componentInstance.reviews().length).toBe(1);
    expect(f.componentInstance.reviews()[0].pr_number).toBe(7);
    expect(f.componentInstance.reviews()[0].status).toBe('pending');
  });

  it('trigger 400 surfaces inline error via mf-err', () => {
    const f = mount();
    setTriggerForm(f, 'ag1', 'c1', 99);

    f.componentInstance.triggerReview();
    mock.expectOne('/api/v1/businesses/b1/code-reviews').flush(
      { error: 'egress not allowlisted' }, { status: 400, statusText: 'Bad Request' },
    );
    f.detectChanges();

    const errEl = f.nativeElement.querySelector('[data-testid="trigger-error"]');
    expect(errEl).toBeTruthy();
    expect(errEl.textContent).toContain('egress not allowlisted');
  });

  it('trigger 404 surfaces inline not-found error', () => {
    const f = mount();
    setTriggerForm(f, 'ag1', 'c1', 5);

    f.componentInstance.triggerReview();
    mock.expectOne('/api/v1/businesses/b1/code-reviews').flush(
      {}, { status: 404, statusText: 'Not Found' },
    );
    f.detectChanges();

    const errEl = f.nativeElement.querySelector('[data-testid="trigger-error"]');
    expect(errEl).toBeTruthy();
    expect(errEl.textContent).toContain('not found');
  });

  // ── Misc ─────────────────────────────────────────────────────────────────────

  it('renders agent options in the cr-agent select', () => {
    const f = mount();
    const select = f.nativeElement.querySelector('[data-testid="cr-agent"]') as HTMLSelectElement;
    expect(select).toBeTruthy();
    // One placeholder + one real option.
    expect(select.options.length).toBeGreaterThanOrEqual(2);
    expect(select.options[1].textContent).toContain('Reviewer Agent');
  });

  it('renders connector options in the cr-connector select', () => {
    const f = mount();
    const select = f.nativeElement.querySelector('[data-testid="cr-connector"]') as HTMLSelectElement;
    expect(select).toBeTruthy();
    expect(select.options.length).toBeGreaterThanOrEqual(2);
    expect(select.options[1].textContent).toContain('acme/api');
  });

  it('shows a toast after connector deletion', () => {
    const f = mount();
    const toastSvc = TestBed.inject(ToastService);

    (f.nativeElement.querySelector('[data-testid="connector-delete"]') as HTMLButtonElement).click();
    f.detectChanges();
    (f.nativeElement.querySelector('[data-testid="connector-delete-yes"]') as HTMLButtonElement).click();
    mock.expectOne({ method: 'DELETE', url: '/api/v1/businesses/b1/repo-connectors/c1' }).flush(null);
    f.detectChanges();

    expect(toastSvc.toasts().some((t) => t.message.includes('deleted'))).toBe(true);
  });
});
