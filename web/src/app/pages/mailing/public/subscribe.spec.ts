import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { ActivatedRoute } from '@angular/router';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { MailingSubscribeComponent } from './subscribe';

describe('MailingSubscribeComponent', () => {
  let fixture: ComponentFixture<MailingSubscribeComponent>;
  let component: MailingSubscribeComponent;
  let http: HttpTestingController;

  beforeEach(() => {
    TestBed.configureTestingModule({
      providers: [
        provideHttpClient(),
        provideHttpClientTesting(),
        {
          provide: ActivatedRoute,
          useValue: {
            snapshot: {
              paramMap: new Map([['key', 'mlp_demo']]),
              queryParamMap: new Map([['name', 'Product updates']]),
            },
          },
        },
      ],
    });
    http = TestBed.inject(HttpTestingController);
    fixture = TestBed.createComponent(MailingSubscribeComponent);
    component = fixture.componentInstance;
    fixture.detectChanges();
  });

  afterEach(() => http.verify());

  it('shows the shared check-inbox state after a uniform acceptance response', () => {
    expect(fixture.nativeElement.textContent).toContain('Join Product updates');
    component.email = 'ada@example.test';
    component.firstName = 'Ada';
    component.submit();
    const request = http.expectOne('/api/v1/mailing/public/mlp_demo/subscribe');
    expect(request.request.body).toEqual({
      email: 'ada@example.test',
      first_name: 'Ada',
      website: '',
    });
    request.flush({ accepted: true });
    fixture.detectChanges();
    expect(fixture.nativeElement.querySelector('[data-testid="mailing-public-done"]')).toBeTruthy();
    expect(fixture.nativeElement.textContent).toContain('Check your inbox');
    expect(fixture.nativeElement.textContent).not.toContain('ada@example.test');
  });

  it('shows retry only for a network failure', () => {
    component.email = 'ada@example.test';
    component.submit();
    http.expectOne('/api/v1/mailing/public/mlp_demo/subscribe').error(new ProgressEvent('network'));
    fixture.detectChanges();
    expect(
      fixture.nativeElement.querySelector('[data-testid="mailing-public-error"]'),
    ).toBeTruthy();
    expect(component.done()).toBe(false);
  });

  it('collapses HTTP failures to the same check-inbox state', () => {
    component.email = 'ada@example.test';
    component.submit();
    http
      .expectOne('/api/v1/mailing/public/mlp_demo/subscribe')
      .flush({ error: 'invalid request' }, { status: 400, statusText: 'Bad Request' });
    fixture.detectChanges();
    expect(fixture.nativeElement.querySelector('[data-testid="mailing-public-done"]')).toBeTruthy();
  });
});
