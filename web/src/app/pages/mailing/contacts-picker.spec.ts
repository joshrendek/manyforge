import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { beforeEach, describe, expect, it } from 'vitest';
import { ContactsPickerComponent } from './contacts-picker';

const BUSINESS_ID = '11111111-1111-4111-8111-111111111111';
const LIST_ID = '22222222-2222-4222-8222-222222222222';
const CONTACT_ID = '33333333-3333-4333-8333-333333333333';

describe('ContactsPickerComponent', () => {
  let http: HttpTestingController;
  let fixture: ComponentFixture<ContactsPickerComponent>;

  beforeEach(() => {
    TestBed.configureTestingModule({
      providers: [provideHttpClient(), provideHttpClientTesting()],
    });
    http = TestBed.inject(HttpTestingController);
    fixture = TestBed.createComponent(ContactsPickerComponent);
    fixture.componentRef.setInput('businessId', BUSINESS_ID);
    fixture.componentRef.setInput('listId', LIST_ID);
    fixture.detectChanges();
    http.expectOne(`/api/v1/businesses/${BUSINESS_ID}/contacts`).flush({
      items: [
        {
          id: CONTACT_ID,
          tenant_root_id: BUSINESS_ID,
          primary_email: 'ada@example.com',
          display_name: 'Ada',
          created_at: '',
          updated_at: '',
        },
      ],
      next_cursor: null,
    });
    fixture.detectChanges();
  });

  it('posts selected contact ids to the from-contacts endpoint', () => {
    const checkbox = fixture.nativeElement.querySelector(
      '[data-testid="contacts-picker-checkbox"]',
    ) as HTMLInputElement;
    checkbox.click();
    fixture.detectChanges();
    fixture.componentInstance.addSelected();
    const request = http.expectOne(
      `/api/v1/businesses/${BUSINESS_ID}/mailing/lists/${LIST_ID}/subscribers/from-contacts`,
    );
    expect(request.request.body).toEqual({ contact_ids: [CONTACT_ID], skip_confirmation: false });
    request.flush({ imported: 1, skipped: 0, errors: [] });
  });
});
