import { HttpErrorResponse } from '@angular/common/http';
import { Component, OnInit, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { BusinessService } from '../../core/business.service';
import { Company, CrmService } from '../../core/crm.service';
import { CurrentBusinessService } from '../../core/current-business.service';
import { Business } from '../../core/tree';
import { EmptyState } from '../../ui/empty-state/empty-state';
import { PageHeader } from '../../ui/page-header/page-header';
import { Spinner } from '../../ui/spinner/spinner';
import { ToastService } from '../../ui/toast/toast.service';

// Companies list page: tenant-wide companies reached through a business-scoped URL.
// Mirrors contacts-list.ts exactly — same business selection (BusinessService +
// CurrentBusinessService), an inline new-company form (name required, domain optional)
// that posts then reloads. List + inline-create only for this slice (no detail page).
@Component({
  selector: 'app-companies-list',
  imports: [FormsModule, PageHeader, EmptyState, Spinner],
  template: `
    <div class="mf-card" data-testid="companies-page">
      <mf-page-header title="Companies" subtitle="Organizations your contacts belong to">
        @if (loading()) {
          <span class="mf-loading-row" data-testid="companies-loading" actions><mf-spinner /></span>
        }
      </mf-page-header>

      <div class="mf-filters">
        <div class="mf-field" style="flex:1 1 220px">
          <label for="crm-co-biz-select">Business</label>
          <select id="crm-co-biz-select" class="mf-select" data-testid="business-select"
                  [ngModel]="businessId()" (ngModelChange)="selectBusiness($event)" name="biz">
            <option value="" disabled>Choose a business…</option>
            @for (b of businesses(); track b.id) {
              <option [value]="b.id">{{ b.is_tenant_root ? b.name + ' (master)' : b.name }}</option>
            }
          </select>
        </div>
      </div>

      @if (businessId()) {
        <form class="mf-filters" data-testid="company-new" (ngSubmit)="create()">
          <div class="mf-field" style="flex:1 1 220px">
            <label for="crm-new-co-name">New company name</label>
            <input id="crm-new-co-name" class="mf-input" type="text" name="newName"
                   placeholder="Acme Inc" [(ngModel)]="newName" />
          </div>
          <div class="mf-field" style="flex:1 1 220px">
            <label for="crm-new-co-domain">Domain (optional)</label>
            <input id="crm-new-co-domain" class="mf-input" type="text" name="newDomain"
                   placeholder="acme.test" [(ngModel)]="newDomain" />
          </div>
          <div style="display:flex;align-items:flex-end">
            <button type="submit" class="mf-btn mf-btn-primary mf-btn-sm" data-testid="company-create"
                    [disabled]="!newName.trim() || creating()">
              {{ creating() ? 'Adding…' : 'Add company' }}
            </button>
          </div>
        </form>
      }

      <div class="mf-table" data-testid="companies-list">
        <div class="mf-tr mf-th">
          <span style="flex:1">Name</span>
          <span style="flex:1">Domain</span>
        </div>
        @for (c of items(); track c.id) {
          <div class="mf-tr" data-testid="company-row" [attr.data-company-id]="c.id">
            <span style="flex:1" data-testid="company-name-cell">{{ c.name }}</span>
            <span style="flex:1" data-testid="company-domain-cell">{{ c.domain || '—' }}</span>
          </div>
        }
        @if (!items().length && businessId() && !loading()) {
          <mf-empty-state title="No companies yet" data-testid="companies-empty">
            Add one with the form above.
          </mf-empty-state>
        }
      </div>

      @if (error()) {
        <p class="mf-err" data-testid="companies-error">{{ error() }}</p>
      }
    </div>
  `,
})
export class CompaniesListComponent implements OnInit {
  private bizApi = inject(BusinessService);
  private crm = inject(CrmService);
  private current = inject(CurrentBusinessService);
  private toast = inject(ToastService);

  businesses = signal<Business[]>([]);
  businessId = signal<string>('');
  items = signal<Company[]>([]);
  loading = signal(false);
  error = signal('');
  newName = '';
  newDomain = '';
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
    this.crm.listCompanies(biz).subscribe({
      next: (r) => {
        if (this.businessId() !== biz) return;
        this.items.set(r.items ?? []);
        this.error.set('');
        this.loading.set(false);
      },
      error: () => {
        if (this.businessId() !== biz) return;
        this.items.set([]);
        this.error.set('Could not load companies');
        this.loading.set(false);
      },
    });
  }

  // company_id-style: domain is omitted from the body when blank rather than sending "".
  create(): void {
    const name = this.newName.trim();
    if (!name || this.creating()) return;
    this.creating.set(true);
    const body: { name: string; domain?: string } = { name };
    const domain = this.newDomain.trim();
    if (domain) body.domain = domain;
    this.crm.createCompany(this.businessId(), body).subscribe({
      next: () => {
        this.newName = '';
        this.newDomain = '';
        this.creating.set(false);
        this.toast.success('Company created');
        this.reload();
      },
      error: (e: HttpErrorResponse) => {
        this.creating.set(false);
        this.toast.error(e.status === 409 ? 'A company with that name already exists' : 'Could not create company');
      },
    });
  }
}
