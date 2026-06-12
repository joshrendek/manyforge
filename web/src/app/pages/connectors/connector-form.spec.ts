import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { TestBed } from '@angular/core/testing';
import { beforeEach, describe, expect, it } from 'vitest';
import { ConnectorFormComponent } from './connector-form';

describe('ConnectorFormComponent', () => {
  let mock: HttpTestingController;
  beforeEach(() => {
    TestBed.configureTestingModule({ providers: [provideHttpClient(), provideHttpClientTesting()] });
    mock = TestBed.inject(HttpTestingController);
  });

  it('credential inputs are type=password (create mode)', () => {
    const f = TestBed.createComponent(ConnectorFormComponent);
    f.componentInstance.businessId = 'b1';
    f.componentInstance.mode = 'create';
    f.detectChanges();
    const el: HTMLElement = f.nativeElement;
    expect((el.querySelector('[data-testid="conn-api-token"]') as HTMLInputElement).type).toBe('password');
    expect((el.querySelector('[data-testid="conn-webhook-secret"]') as HTMLInputElement).type).toBe('password');
  });

  it('create submit POSTs the body and emits saved', () => {
    const f = TestBed.createComponent(ConnectorFormComponent);
    const c = f.componentInstance;
    c.businessId = 'b1';
    c.mode = 'create';
    let saved = false;
    c.saved.subscribe(() => (saved = true));
    c.displayName = 'Acme';
    c.baseUrl = 'https://acme.atlassian.net';
    c.email = 'a@b.c';
    c.apiToken = 'tok';
    f.detectChanges();
    c.submit();
    const req = mock.expectOne('/api/v1/businesses/b1/connectors');
    expect(req.request.method).toBe('POST');
    expect(req.request.body.api_token).toBe('tok');
    req.flush({ id: 'c1' });
    expect(saved).toBe(true);
  });

  it('rotate mode only shows credential fields and PUTs to /credential', () => {
    const f = TestBed.createComponent(ConnectorFormComponent);
    const c = f.componentInstance;
    c.businessId = 'b1';
    c.mode = 'rotate';
    c.connectorId = 'c1';
    f.detectChanges();
    expect(f.nativeElement.querySelector('[data-testid="conn-display-name"]')).toBeNull();
    c.email = 'a@b.c';
    c.apiToken = 'newtok';
    c.submit();
    const req = mock.expectOne('/api/v1/businesses/b1/connectors/c1/credential');
    expect(req.request.method).toBe('PUT');
    req.flush({ id: 'c1' });
  });

  it('edit mode prefills from connector, hides credential/base_url fields, and PATCHes on submit', () => {
    const f = TestBed.createComponent(ConnectorFormComponent);
    const comp = f.componentInstance;
    comp.businessId = 'b1';
    comp.mode = 'edit';
    comp.connectorId = 'c1';
    comp.connector = {
      id: 'c1', business_id: 'b1', type: 'jira', display_name: 'Acme Jira',
      base_url: 'https://acme.atlassian.net', allow_private_base_url: false,
      config: { project_key: 'PROJ', issue_type: 'Bug' },
      status: 'enabled', last_reconciled_at: null,
      created_at: '2026-06-12T00:00:00Z', updated_at: '2026-06-12T00:00:00Z',
      health: { state: 'healthy', linked_ticket_count: 0, pending_outbound_ops: 0, failed_outbound_ops: 0, last_error: null },
    };
    f.detectChanges();
    const el: HTMLElement = f.nativeElement;

    // Prefill check
    expect(comp.displayName).toBe('Acme Jira');
    expect(comp.projectKey).toBe('PROJ');
    expect(comp.issueType).toBe('Bug');

    // Credential + base_url fields must not be present
    expect(el.querySelector('[data-testid="conn-api-token"]')).toBeNull();
    expect(el.querySelector('[data-testid="conn-webhook-secret"]')).toBeNull();
    expect(el.querySelector('[data-testid="conn-base-url"]')).toBeNull();

    // Submit button label should be "Save"
    expect((el.querySelector('[data-testid="connector-form-submit"]') as HTMLButtonElement).textContent?.trim()).toBe('Save');

    // Submit emits saved and PATCHes
    let saved = false;
    comp.saved.subscribe(() => (saved = true));
    comp.submit();
    const req = mock.expectOne('/api/v1/businesses/b1/connectors/c1');
    expect(req.request.method).toBe('PATCH');
    expect(req.request.body.display_name).toBe('Acme Jira');
    expect(req.request.body.config).toEqual({ project_key: 'PROJ', issue_type: 'Bug' });
    req.flush({ id: 'c1' });
    expect(saved).toBe(true);
  });
});
