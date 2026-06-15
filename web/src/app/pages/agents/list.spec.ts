import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { provideRouter } from '@angular/router';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { ToastService } from '../../ui/toast/toast.service';
import { AgentsListComponent } from './list';

const biz = {
  items: [{ id: 'b1', parent_id: null, tenant_root_id: 'b1', name: 'Acme', status: 'active', is_tenant_root: true }],
  next_cursor: null,
};
const agents = {
  items: [
    {
      id: 'a1', business_id: 'b1', principal_id: 'p1', name: 'Triage', provider: 'anthropic', model: 'claude-opus-4-8',
      system_prompt: '', allowed_tools: [], autonomy_mode: 1, enabled: true, monthly_budget_cents: 2500,
      allowed_mcp_servers: [], retriage_on_reply: false, created_at: '', updated_at: '',
    },
  ],
};

describe('AgentsListComponent', () => {
  let mock: HttpTestingController;
  beforeEach(() => {
    localStorage.clear();
    TestBed.configureTestingModule({ providers: [provideHttpClient(), provideHttpClientTesting(), provideRouter([])] });
    mock = TestBed.inject(HttpTestingController);
  });
  afterEach(() => { document.documentElement.setAttribute('data-theme', 'light'); localStorage.clear(); });

  // Basic list mount: businesses, then agents. The add/edit form is closed, so the form's own
  // metadata GETs (/agents/tools, /agents/models, /mcp_servers) do not fire.
  function mount(): ComponentFixture<AgentsListComponent> {
    const f = TestBed.createComponent(AgentsListComponent);
    f.detectChanges();
    mock.expectOne('/api/v1/businesses').flush(biz);
    f.detectChanges();
    mock.expectOne('/api/v1/businesses/b1/agents').flush(agents);
    f.detectChanges();
    return f;
  }

  it('loads businesses then lists agents', () => {
    const f = mount();
    expect(f.componentInstance.items().length).toBe(1);
    expect(f.componentInstance.items()[0].name).toBe('Triage');
  });

  it('renders an agent row with its name', () => {
    const el: HTMLElement = mount().nativeElement;
    expect(el.querySelector('[data-testid="agent-row"]')).toBeTruthy();
    expect(el.querySelector('[data-testid="agent-name-cell"]')?.textContent).toContain('Triage');
  });

  it('shows the autonomy label, enabled flag, and budget in dollars', () => {
    const el: HTMLElement = mount().nativeElement;
    const row = el.querySelector('[data-testid="agent-row"]') as HTMLElement;
    expect(row.textContent).toContain('Assist');
    expect(row.textContent).toContain('yes');
    expect(row.textContent).toContain('$25');
  });

  it('delete asks for confirm, then DELETEs and removes the row', () => {
    const f = mount();
    (f.nativeElement.querySelector('[data-testid="agent-delete"]') as HTMLButtonElement).click();
    f.detectChanges();
    expect(f.nativeElement.querySelector('[data-testid="agent-delete-confirm"]')).toBeTruthy();
    (f.nativeElement.querySelector('[data-testid="agent-delete-yes"]') as HTMLButtonElement).click();
    mock.expectOne('/api/v1/businesses/b1/agents/a1').flush(null);
    f.detectChanges();
    expect(f.nativeElement.querySelector('[data-testid="agent-row"]')).toBeNull();
  });

  it('cancelling a delete confirm leaves the row in place', () => {
    const f = mount();
    (f.nativeElement.querySelector('[data-testid="agent-delete"]') as HTMLButtonElement).click();
    f.detectChanges();
    (f.nativeElement.querySelector('[data-testid="agent-delete-no"]') as HTMLButtonElement).click();
    f.detectChanges();
    expect(f.nativeElement.querySelector('[data-testid="agent-delete-confirm"]')).toBeNull();
    expect(f.nativeElement.querySelector('[data-testid="agent-row"]')).toBeTruthy();
  });

  it('clicking Edit swaps in an inline edit form for that agent', () => {
    const f = mount();
    (f.nativeElement.querySelector('[data-testid="agent-edit"]') as HTMLButtonElement).click();
    f.detectChanges();
    // The edit form renders mode="edit"; its ngOnInit fires the metadata GETs — flush them.
    mock.expectOne('/api/v1/businesses/b1/agents/tools').flush({ items: [] });
    mock.expectOne('/api/v1/businesses/b1/agents/models').flush({ items: [] });
    mock.expectOne('/api/v1/businesses/b1/mcp_servers').flush({ items: [] });
    f.detectChanges();
    expect(f.componentInstance.editId()).toBe('a1');
    expect(f.nativeElement.querySelector('[data-testid="agent-edit-row"]')).toBeTruthy();
    expect(f.nativeElement.querySelector('[data-testid="agent-form"]')).toBeTruthy();
  });

  it('toggles the add form via the add toggle', () => {
    const f = mount();
    const el: HTMLElement = f.nativeElement;
    expect(el.querySelector('[data-testid="agent-form"]')).toBeNull();
    (el.querySelector('[data-testid="agent-add-toggle"]') as HTMLButtonElement).click();
    f.detectChanges();
    // Opening the create form fires its metadata GETs — flush them.
    mock.expectOne('/api/v1/businesses/b1/agents/tools').flush({ items: [] });
    mock.expectOne('/api/v1/businesses/b1/agents/models').flush({ items: [] });
    mock.expectOne('/api/v1/businesses/b1/mcp_servers').flush({ items: [] });
    f.detectChanges();
    expect(el.querySelector('[data-testid="agent-form"]')).toBeTruthy();
  });

  it('shows a success toast after an agent is created', () => {
    const f = mount();
    const toastSvc = TestBed.inject(ToastService);
    f.componentInstance.onCreated();
    mock.expectOne('/api/v1/businesses/b1/agents').flush(agents);
    f.detectChanges();
    expect(toastSvc.toasts().some((t) => t.message.includes('Agent created'))).toBe(true);
  });

  it('renders in dark theme', () => {
    document.documentElement.setAttribute('data-theme', 'dark');
    expect(mount().nativeElement.querySelector('.mf-table, .mf-card')).toBeTruthy();
  });
});
