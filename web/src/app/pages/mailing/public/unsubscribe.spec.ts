import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { ActivatedRoute } from '@angular/router';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { MailingUnsubscribeComponent } from './unsubscribe';

describe('MailingUnsubscribeComponent', () => {
  let fixture: ComponentFixture<MailingUnsubscribeComponent>;
  let component: MailingUnsubscribeComponent;
  let http: HttpTestingController;

  beforeEach(() => {
    TestBed.configureTestingModule({
      providers: [
        provideHttpClient(),
        provideHttpClientTesting(),
        {
          provide: ActivatedRoute,
          useValue: { snapshot: { paramMap: new Map([['token', 'unsubscribe-token']]) } },
        },
      ],
    });
    http = TestBed.inject(HttpTestingController);
    fixture = TestBed.createComponent(MailingUnsubscribeComponent);
    component = fixture.componentInstance;
    fixture.detectChanges();
  });

  afterEach(() => http.verify());

  it('shows the shared non-oracle done card after unsubscribe', () => {
    component.submit();
    http.expectOne('/m/u/unsubscribe-token').flush('');
    fixture.detectChanges();
    const done = fixture.nativeElement.querySelector('[data-testid="mailing-public-done"]');
    expect(done?.textContent).toContain('All set');
    expect(done?.textContent).toContain('Your request has been processed.');
    expect(done?.textContent).not.toContain('unsubscribe-token');
  });
});
