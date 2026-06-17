import { HttpErrorResponse } from '@angular/common/http';
import { Component, OnInit, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { RouterLink } from '@angular/router';
import { BusinessService } from '../../core/business.service';
import { Contact, CrmService } from '../../core/crm.service';
import { CurrentBusinessService } from '../../core/current-business.service';
import { Business } from '../../core/tree';
import { EmptyState } from '../../ui/empty-state/empty-state';
import { PageHeader } from '../../ui/page-header/page-header';
import { Spinner } from '../../ui/spinner/spinner';
import { ToastService } from '../../ui/toast/toast.service';

// Contacts list page: tenant-wide contacts reached through a business-scoped URL. Business
// selection mirrors the agents page (same /api/v1/businesses list, persisted via
// CurrentBusinessService). An inline new-contact form posts a primary_email then reloads.
@Component({
  selector: 'app-contacts-list',
  imports: [FormsModule, RouterLink, PageHeader, EmptyState, Spinner],
  template: `
    <div class="mf-card" data-testid="contacts-page">
      <mf-page-header title="Contacts" subtitle="People you correspond with across your businesses">
        @if (loading()) {
          <span class="mf-loading-row" data-testid="contacts-loading" actions><mf-spinner /></span>
        }
      </mf-page-header>

      <div class="mf-filters">
        <div class="mf-field" style="flex:1 1 220px">
          <label for="crm-biz-select">Business</label>
          <select id="crm-biz-select" class="mf-select" data-testid="business-select"
                  [ngModel]="businessId()" (ngModelChange)="selectBusiness($event)" name="biz">
            <option value="" disabled>Choose a business…</option>
            @for (b of businesses(); track b.id) {
              <option [value]="b.id">{{ b.is_tenant_root ? b.name + ' (master)' : b.name }}</option>
            }
          </select>
        </div>
      </div>

      @if (businessId()) {
        <form class="mf-filters" data-testid="contact-new" (ngSubmit)="create()">
          <div class="mf-field" style="flex:1 1 260px">
            <label for="crm-new-email">New contact email</label>
            <input id="crm-new-email" class="mf-input" type="email" name="newEmail"
                   placeholder="person@example.com" [(ngModel)]="newEmail" />
          </div>
          <div style="display:flex;align-items:flex-end">
            <button type="submit" class="mf-btn mf-btn-primary mf-btn-sm" data-testid="contact-create"
                    [disabled]="!newEmail.trim() || creating()">
              {{ creating() ? 'Adding…' : 'Add contact' }}
            </button>
          </div>
        </form>
      }

      <div class="mf-table" data-testid="contacts-list">
        <div class="mf-tr mf-th">
          <span style="flex:1">Email</span>
          <span style="flex:1">Name</span>
        </div>
        @for (c of items(); track c.id) {
          <div class="mf-tr" data-testid="contact-row" [attr.data-contact-id]="c.id">
            <span style="flex:1" data-testid="contact-email-cell">
              <a [routerLink]="['/crm', businessId(), 'contacts', c.id]">{{ c.primary_email }}</a>
            </span>
            <span style="flex:1" data-testid="contact-name-cell">{{ c.display_name || '—' }}</span>
          </div>
        }
        @if (!items().length && businessId() && !loading()) {
          <mf-empty-state title="No contacts yet" data-testid="contacts-empty">
            Add one with the form above.
          </mf-empty-state>
        }
      </div>

      @if (error()) {
        <p class="mf-err" data-testid="contacts-error">{{ error() }}</p>
      }
    </div>
  `,
})
export class ContactsListComponent implements OnInit {
  private bizApi = inject(BusinessService);
  private crm = inject(CrmService);
  private current = inject(CurrentBusinessService);
  private toast = inject(ToastService);

  businesses = signal<Business[]>([]);
  businessId = signal<string>('');
  items = signal<Contact[]>([]);
  loading = signal(false);
  error = signal('');
  newEmail = '';
  creating = signal(false);

  ngOnInit(): void {
    this.bizApi.list().subscribe({
      next: (r) => {
        const items = r.items ?? [];
        this.businesses.set(items);
        const id = this.current.businessId() ?? items[0]?.id;
        if (id) {
          this.businessId.set(id);
          this.current.set(id);
          this.reload();
        }
      },
      error: () => this.error.set('Could not load businesses'),
    });
  }

  selectBusiness(id: string): void {
    this.businessId.set(id);
    this.current.set(id);
    this.reload();
  }

  reload(): void {
    if (!this.businessId()) return;
    const biz = this.businessId();
    this.loading.set(true);
    this.crm.listContacts(biz).subscribe({
      next: (r) => {
        if (this.businessId() !== biz) return;
        this.items.set(r.items ?? []);
        this.error.set('');
        this.loading.set(false);
      },
      error: () => {
        if (this.businessId() !== biz) return;
        this.items.set([]);
        this.error.set('Could not load contacts');
        this.loading.set(false);
      },
    });
  }

  create(): void {
    const email = this.newEmail.trim();
    if (!email || this.creating()) return;
    this.creating.set(true);
    this.crm.createContact(this.businessId(), { primary_email: email }).subscribe({
      next: () => {
        this.newEmail = '';
        this.creating.set(false);
        this.toast.success('Contact created');
        this.reload();
      },
      error: (e: HttpErrorResponse) => {
        this.creating.set(false);
        this.toast.error(e.status === 409 ? 'A contact with that email already exists' : 'Could not create contact');
      },
    });
  }
}
