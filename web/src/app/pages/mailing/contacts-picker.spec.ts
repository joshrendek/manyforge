import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { beforeEach, describe, expect, it } from 'vitest';
import { ContactsPickerComponent } from './contacts-picker';

describe('ContactsPickerComponent', () => {
  let http: HttpTestingController;
  let fixture: ComponentFixture<ContactsPickerComponent>;

  beforeEach(() => {
    TestBed.configureTestingModule({
      providers: [provideHttpClient(), provideHttpClientTesting()],
    });
    http = TestBed.inject(HttpTestingController);
    fixture = TestBed.createComponent(ContactsPickerComponent);
    fixture.componentRef.setInput('businessId', 'b1');
    fixture.componentRef.setInput('listId', 'l1');
    fixture.detectChanges();
    http.expectOne('/api/v1/businesses/b1/contacts').flush({
      items: [
        {
          id: 'c1',
          tenant_root_id: 'b1',
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
      '/api/v1/businesses/b1/mailing/lists/l1/subscribers/from-contacts',
    );
    expect(request.request.body).toEqual({ contact_ids: ['c1'], skip_confirmation: false });
    request.flush({ imported: 1, skipped: 0, errors: [] });
  });
});
