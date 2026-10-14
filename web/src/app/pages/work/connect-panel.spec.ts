import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { WorkConnectPanelComponent } from './connect-panel';
import { makeProject } from './work-fixtures';

const tokensUrl = '/api/v1/businesses/b1/work/projects/p1/tokens';
const token = {
  id: 't1',
  name: 'laptop',
  prefix: 'mfw_ab12',
  created_at: '2026-10-01T00:00:00Z',
  last_used_at: '2026-10-13T09:00:00Z',
  revoked_at: null,
};

describe('WorkConnectPanelComponent', () => {
  let fixture: ComponentFixture<WorkConnectPanelComponent>;
  let cmp: WorkConnectPanelComponent;
  let mock: HttpTestingController;

  function mount(items = [token]): void {
    fixture = TestBed.createComponent(WorkConnectPanelComponent);
    cmp = fixture.componentInstance;
    fixture.componentRef.setInput('businessId', 'b1');
    fixture.componentRef.setInput('project', makeProject());
    fixture.detectChanges();
    mock.expectOne(tokensUrl).flush({ items });
    fixture.detectChanges();
  }

  function q(sel: string): HTMLElement | null {
    return fixture.nativeElement.querySelector(sel) as HTMLElement | null;
  }

  beforeEach(() => {
    TestBed.configureTestingModule({
      providers: [provideHttpClient(), provideHttpClientTesting()],
    });
    mock = TestBed.inject(HttpTestingController);
  });
  afterEach(() => mock.verify());

  it('renders setup commands for this server and project', () => {
    mount();
    const origin = window.location.origin;
    expect(q('[data-testid="work-cmd-install"]')?.textContent).toBe(
      `curl -fsSL ${origin}/cli/install.sh | sh`,
    );
    expect(q('[data-testid="work-cmd-init"]')?.textContent).toBe(
      `mf init --server ${origin} --project manyforge`,
    );
    expect(q('[data-testid="work-cmd-login"]')?.textContent).toBe(
      `mf login --server ${origin} --token mfw_…`,
    );
    expect(q('[data-testid="work-cmd-setup"]')?.textContent).toBe('mf setup claude');
    expect(q('[data-testid="work-connect"]')?.textContent).toContain('make install-mf');
  });

  it('lists tokens by prefix, never a plaintext secret', () => {
    mount();
    const row = q('[data-testid="work-token-row"]');
    expect(row?.textContent).toContain('laptop');
    expect(row?.textContent).toContain('mfw_ab12');
    expect(q('[data-testid="work-token-once"]')).toBeNull();
  });

  it('creating a token shows the plaintext once with the full login command', () => {
    mount([]);
    cmp.newName = 'ci';
    cmp.create();
    const req = mock.expectOne(tokensUrl);
    expect(req.request.method).toBe('POST');
    expect(req.request.body).toEqual({ name: 'ci' });
    req.flush({
      id: 't2',
      name: 'ci',
      prefix: 'mfw_zz99',
      created_at: '2026-10-14T00:00:00Z',
      token: 'mfw_zz99secret',
    });
    fixture.detectChanges();
    expect(q('[data-testid="work-token-login-cmd"]')?.textContent).toBe(
      `mf login --server ${window.location.origin} --token mfw_zz99secret`,
    );
    // The new row carries the prefix only.
    expect(q('[data-testid="work-token-row"]')?.textContent).not.toContain('secret');

    (q('[data-testid="work-token-dismiss"]') as HTMLButtonElement).click();
    fixture.detectChanges();
    expect(q('[data-testid="work-token-once"]')).toBeNull();
    expect(fixture.nativeElement.textContent).not.toContain('mfw_zz99secret');
  });

  it('revokes a token after confirmation with DELETE', () => {
    mount();
    (q('[data-testid="work-token-revoke"]') as HTMLButtonElement).click();
    fixture.detectChanges();
    (q('[data-testid="work-token-revoke-confirm"]') as HTMLButtonElement).click();
    const req = mock.expectOne(`${tokensUrl}/t1`);
    expect(req.request.method).toBe('DELETE');
    req.flush(null, { status: 204, statusText: 'No Content' });
    fixture.detectChanges();
    expect(cmp.tokens()[0].revoked_at).not.toBeNull();
    expect(q('[data-testid="work-token-row"]')?.textContent).toContain('Revoked');
  });

  it('hides token management from members without work.write (list 404s)', () => {
    fixture = TestBed.createComponent(WorkConnectPanelComponent);
    fixture.componentRef.setInput('businessId', 'b1');
    fixture.componentRef.setInput('project', makeProject());
    fixture.detectChanges();
    mock.expectOne(tokensUrl).flush({ message: 'not found' }, { status: 404, statusText: 'NF' });
    fixture.detectChanges();
    expect(q('[data-testid="work-tokens"]')).toBeNull();
    expect(q('[data-testid="work-cmd-init"]')).toBeTruthy();
    expect(q('[data-testid="work-connect"]')?.textContent).toContain('Ask a project admin');
  });
});
