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
});
