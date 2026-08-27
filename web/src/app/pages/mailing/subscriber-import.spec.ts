import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { beforeEach, describe, expect, it } from 'vitest';
import { SubscriberImportComponent } from './subscriber-import';

describe('SubscriberImportComponent', () => {
  let http: HttpTestingController;
  let fixture: ComponentFixture<SubscriberImportComponent>;

  beforeEach(() => {
    TestBed.configureTestingModule({
      providers: [provideHttpClient(), provideHttpClientTesting()],
    });
    http = TestBed.inject(HttpTestingController);
    fixture = TestBed.createComponent(SubscriberImportComponent);
    fixture.componentRef.setInput('businessId', 'b1');
    fixture.componentRef.setInput('listId', 'l1');
    fixture.detectChanges();
  });

  it('gates import on both a file and explicit consent attestation', () => {
    const submit = fixture.nativeElement.querySelector(
      '[data-testid="subscriber-import-submit"]',
    ) as HTMLButtonElement;
    expect(submit.disabled).toBe(true);
    fixture.componentInstance.file = new File(['email'], 'people.csv');
    fixture.detectChanges();
    expect(submit.disabled).toBe(true);
    (
      fixture.nativeElement.querySelector(
        '[data-testid="subscriber-import-consent"]',
      ) as HTMLInputElement
    ).click();
    fixture.detectChanges();
    expect(submit.disabled).toBe(false);
  });

  it('shows synchronous import counts', () => {
    fixture.componentInstance.file = new File(['email'], 'people.csv');
    fixture.componentInstance.consentAttested = true;
    fixture.componentInstance.submit();
    http.expectOne('/api/v1/businesses/b1/mailing/lists/l1/subscribers/import').flush({
      imported: 3,
      skipped: 1,
      errors: [],
    });
    fixture.detectChanges();
    expect(
      fixture.nativeElement.querySelector('[data-testid="subscriber-import-result"]')?.textContent,
    ).toContain('Imported 3; skipped 1');
  });
});
