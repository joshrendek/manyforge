import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { ActivatedRoute } from '@angular/router';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { MailingConfirmComponent } from './confirm';

describe('MailingConfirmComponent', () => {
  let fixture: ComponentFixture<MailingConfirmComponent>;
  let component: MailingConfirmComponent;
  let http: HttpTestingController;

  beforeEach(() => {
    TestBed.configureTestingModule({
      providers: [
        provideHttpClient(),
        provideHttpClientTesting(),
        {
          provide: ActivatedRoute,
          useValue: { snapshot: { paramMap: new Map([['token', 'confirm-token']]) } },
        },
      ],
    });
    http = TestBed.inject(HttpTestingController);
    fixture = TestBed.createComponent(MailingConfirmComponent);
    component = fixture.componentInstance;
    fixture.detectChanges();
  });

  afterEach(() => http.verify());

  it('shows the same non-oracle done card even when the POST fails', () => {
    component.submit();
    http
      .expectOne('/m/confirm/confirm-token')
      .flush('failure', { status: 500, statusText: 'Server Error' });
    fixture.detectChanges();
    const done = fixture.nativeElement.querySelector('[data-testid="mailing-public-done"]');
    expect(done?.textContent).toContain('All set');
    expect(done?.textContent).toContain('Your request has been processed.');
    expect(done?.textContent).not.toContain('confirm-token');
  });
});
