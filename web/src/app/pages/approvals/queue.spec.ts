import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { TestBed } from '@angular/core/testing';
import { provideRouter } from '@angular/router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApprovalsQueueComponent } from './queue';

const biz = { items: [{ id: 'b1', parent_id: null, tenant_root_id: 'b1', name: 'Acme', status: 'active', is_tenant_root: true }] };
const approvals = { items: [{ id: 'a1', agent_run_id: 'r1', tool: 'add_external_comment', effect_class: 2, state: 'pending', expires_at: '2026-07-01T00:00:00Z', summary: 'Comment on ticket 7bbeb32e: "Hi"' }] };

describe('ApprovalsQueueComponent', () => {
  let mock: HttpTestingController;
  beforeEach(() => {
    vi.useFakeTimers();
    localStorage.clear();
    TestBed.configureTestingModule({ providers: [provideHttpClient(), provideHttpClientTesting(), provideRouter([])] });
    mock = TestBed.inject(HttpTestingController);
  });
  afterEach(() => { vi.useRealTimers(); document.documentElement.setAttribute('data-theme', 'light'); localStorage.clear(); });

  function mount() {
    const f = TestBed.createComponent(ApprovalsQueueComponent);
    f.detectChanges();
    mock.expectOne('/api/v1/businesses').flush(biz);
    f.detectChanges();
    mock.expectOne('/api/v1/businesses/b1/approvals').flush(approvals);
    f.detectChanges();
    return f;
  }

  it('renders rows with summary + effect badge', () => {
    const el: HTMLElement = mount().nativeElement;
    expect(el.querySelector('[data-testid="approvals-list"]')).toBeTruthy();
    expect(el.querySelector('[data-testid="approval-summary"]')?.textContent).toContain('Comment on ticket');
    expect(el.querySelector('[data-testid="approval-effect"] .mf-pill-warn')).toBeTruthy();
  });

  it('approve removes the row', () => {
    const f = mount();
    (f.nativeElement.querySelector('[data-testid="approval-approve"]') as HTMLButtonElement).click();
    mock.expectOne('/api/v1/businesses/b1/approvals/a1/approve').flush({ id: 'a1', state: 'approved' });
    f.detectChanges();
    expect(f.nativeElement.querySelector('[data-testid="approval-row"]')).toBeNull();
  });

  it('renders in dark theme', () => {
    document.documentElement.setAttribute('data-theme', 'dark');
    expect(mount().nativeElement.querySelector('.mf-table, .mf-card')).toBeTruthy();
  });
});
