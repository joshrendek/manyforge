import { HttpErrorResponse } from '@angular/common/http';
import { Component, OnInit, computed, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { ActivatedRoute, Router, RouterLink } from '@angular/router';
import { Company, Contact, CrmService } from '../../core/crm.service';
import { PageHeader } from '../../ui/page-header/page-header';
import { Spinner } from '../../ui/spinner/spinner';
import { ToastService } from '../../ui/toast/toast.service';

// Contact-detail page (/crm/:businessId/contacts/:id). Reads businessId + id from
// the route (mirrors thread-view.ts). On init it loads the contact (header), the
// tenant's companies (the company-assignment picker), and all contacts (the merge
// picker, with this contact excluded). Three mutating blocks — edit, merge, delete —
// each reflect their result into the UI then reload or navigate, never leaving stale
// state, following thread-view's triage-card pattern.
@Component({
  selector: 'app-contact-detail',
  imports: [FormsModule, RouterLink, PageHeader, Spinner],
  template: `
    <div class="mf-card" data-testid="contact-detail">
      <mf-page-header title="Contact">
        <ng-container actions>
          <a class="mf-btn mf-btn-ghost mf-btn-sm" routerLink="/crm/contacts" data-testid="back-to-contacts"
            >Back to contacts</a
          >
        </ng-container>
      </mf-page-header>

      @if (loading()) {
        <div class="mf-loading-row" data-testid="contact-detail-loading">
          <mf-spinner />
          <span>Loading contact…</span>
        </div>
      } @else if (error()) {
        <div class="mf-empty-inline">
          <p data-testid="contact-detail-error">{{ error() }}</p>
          <button class="mf-btn mf-btn-ghost mf-btn-sm" (click)="reload()">Try again</button>
        </div>
      } @else if (contact(); as c) {
        <header class="contact-head">
          <h1 class="contact-email" data-testid="contact-detail-email">{{ c.primary_email }}</h1>
          <p class="contact-name" data-testid="contact-detail-name">{{ c.display_name || '—' }}</p>
        </header>

        <!-- Edit form: display_name + company assignment. The "— none —" option
             leaves the company unchanged (the backend PATCH cannot NULL company_id),
             so we omit company_id from the body when blank rather than sending "". -->
        <form class="mf-card edit-block" data-testid="contact-edit" (ngSubmit)="save()">
          <div class="mf-field">
            <label for="cd-name">Display name</label>
            <input id="cd-name" class="mf-input" type="text" name="name" data-testid="contact-name-input"
                   [(ngModel)]="name" placeholder="Full name" />
          </div>
          <div class="mf-field">
            <label for="cd-company">Company</label>
            <select id="cd-company" class="mf-select" name="company" data-testid="contact-company-select"
                    [(ngModel)]="companyId">
              <option value="">— none —</option>
              @for (co of companies(); track co.id) {
                <option [value]="co.id">{{ co.name }}</option>
              }
            </select>
          </div>
          <div style="display:flex;align-items:flex-end">
            <button type="submit" class="mf-btn mf-btn-primary mf-btn-sm" data-testid="contact-save"
                    [disabled]="saving()">
              {{ saving() ? 'Saving…' : 'Save' }}
            </button>
          </div>
        </form>

        <!-- Merge block: this contact is the WINNER. Pick another (loser) contact to
             merge into this one; on success the loser is gone, so we return to the list. -->
        <div class="mf-card merge-block" data-testid="contact-merge">
          <h2 class="block-title">Merge another contact into this one</h2>
          @if (otherContacts().length === 0) {
            <span class="mf-hint" data-testid="contact-merge-none">No other contacts to merge.</span>
          } @else {
            <div class="mf-filters">
              <div class="mf-field" style="flex:1 1 260px">
                <label for="cd-merge">Contact to merge (loser)</label>
                <select id="cd-merge" class="mf-select" name="loser" data-testid="contact-merge-select"
                        [(ngModel)]="selectedLoserId">
                  <option value="">Choose a contact…</option>
                  @for (o of otherContacts(); track o.id) {
                    <option [value]="o.id">{{ o.primary_email }}</option>
                  }
                </select>
              </div>
              <div style="display:flex;align-items:flex-end">
                <button type="button" class="mf-btn mf-btn-primary mf-btn-sm" data-testid="contact-merge-btn"
                        [disabled]="!selectedLoserId || merging()" (click)="merge()">
                  {{ merging() ? 'Merging…' : 'Merge selected into this contact' }}
                </button>
              </div>
            </div>
          }
        </div>

        <!-- Delete block. -->
        <div class="mf-card delete-block" data-testid="contact-delete">
          <button type="button" class="mf-btn mf-btn-ghost mf-btn-sm" data-testid="contact-delete-btn"
                  [disabled]="deleting()" (click)="remove()">
            {{ deleting() ? 'Deleting…' : 'Delete contact' }}
          </button>
        </div>
      }
    </div>
  `,
  styles: [
    `
      .mf-loading-row {
        display: flex;
        align-items: center;
        gap: 10px;
        color: var(--mf-text-muted);
        font-size: var(--mf-fs-sm);
        padding: 18px 0;
      }
      .mf-empty-inline {
        color: var(--mf-text-muted);
        font-size: var(--mf-fs-base);
        padding: 18px 0;
        display: flex;
        flex-direction: column;
        align-items: flex-start;
        gap: 12px;
      }
      .contact-head {
        margin-bottom: 18px;
      }
      .contact-email {
        font-size: var(--mf-fs-xl);
        font-weight: 680;
        letter-spacing: -0.02em;
        margin: 0;
      }
      .contact-name {
        color: var(--mf-text-muted);
        font-size: var(--mf-fs-sm);
        margin: 6px 0 0;
      }
      .edit-block,
      .merge-block,
      .delete-block {
        margin-top: 16px;
        display: flex;
        flex-direction: column;
        gap: 14px;
      }
      .block-title {
        font-size: var(--mf-fs-base);
        font-weight: 640;
        margin: 0;
      }
    `,
  ],
})
export class ContactDetailComponent implements OnInit {
  private route = inject(ActivatedRoute);
  private router = inject(Router);
  private crm = inject(CrmService);
  private toast = inject(ToastService);

  private businessId = '';
  private id = '';

  contact = signal<Contact | null>(null);
  companies = signal<Company[]>([]);
  // All tenant contacts loaded for the merge picker; this contact is filtered out
  // so it can never be selected as its own loser.
  allContacts = signal<Contact[]>([]);
  otherContacts = computed(() => this.allContacts().filter((c) => c.id !== this.id));

  loading = signal(true);
  error = signal('');
  saving = signal(false);
  merging = signal(false);
  deleting = signal(false);

  // Edit-form fields (plain strings, bound via [(ngModel)] like agent-form).
  name = '';
  companyId = '';
  selectedLoserId = '';

  ngOnInit(): void {
    this.businessId = this.route.snapshot.paramMap.get('businessId') ?? '';
    this.id = this.route.snapshot.paramMap.get('id') ?? '';
    // Best-effort pickers: a failure just leaves the option lists empty.
    if (this.businessId) {
      this.crm.listCompanies(this.businessId).subscribe({
        next: (r) => this.companies.set(r.items ?? []),
        error: () => {},
      });
      this.crm.listContacts(this.businessId).subscribe({
        next: (r) => this.allContacts.set(r.items ?? []),
        error: () => {},
      });
    }
    this.reload();
  }

  reload(): void {
    if (!this.businessId || !this.id) {
      this.loading.set(false);
      this.error.set("We couldn't load this contact.");
      return;
    }
    this.loading.set(true);
    this.error.set('');
    this.crm.getContact(this.businessId, this.id).subscribe({
      next: (c) => {
        this.contact.set(c);
        // Seed the edit form from the loaded contact.
        this.name = c.display_name ?? '';
        this.companyId = c.company_id ?? '';
        this.loading.set(false);
      },
      error: (e: HttpErrorResponse) => {
        this.loading.set(false);
        this.error.set(this.describeLoad(e));
      },
    });
  }

  // Save the editable fields. company_id is omitted when blank (the "— none —"
  // option): the backend PATCH cannot NULL company_id, so a blank selection simply
  // leaves the current company unchanged rather than sending an empty string.
  save(): void {
    if (this.saving()) return;
    this.saving.set(true);
    const body: Partial<{ display_name: string; company_id: string }> = {
      display_name: this.name.trim(),
    };
    if (this.companyId) body.company_id = this.companyId;
    this.crm.updateContact(this.businessId, this.id, body).subscribe({
      next: () => {
        this.saving.set(false);
        this.toast.success('Contact saved');
        this.reload();
      },
      error: (e: HttpErrorResponse) => {
        this.saving.set(false);
        this.toast.error(this.describeMutation(e, 'Could not save the contact'));
      },
    });
  }

  // Merge the selected loser INTO this (winner) contact. On success the loser is
  // gone, so we navigate back to the list.
  merge(): void {
    const loserId = this.selectedLoserId;
    if (!loserId || this.merging()) return;
    this.merging.set(true);
    this.crm.mergeContact(this.businessId, this.id, loserId).subscribe({
      next: () => {
        this.merging.set(false);
        this.toast.success('Merged the selected contact into this one');
        void this.router.navigate(['/crm/contacts']);
      },
      error: (e: HttpErrorResponse) => {
        this.merging.set(false);
        this.toast.error(this.describeMutation(e, 'Could not merge the contacts'));
      },
    });
  }

  remove(): void {
    if (this.deleting()) return;
    this.deleting.set(true);
    this.crm.deleteContact(this.businessId, this.id).subscribe({
      next: () => {
        this.deleting.set(false);
        this.toast.success('Contact deleted');
        void this.router.navigate(['/crm/contacts']);
      },
      error: (e: HttpErrorResponse) => {
        this.deleting.set(false);
        this.toast.error(this.describeMutation(e, 'Could not delete the contact'));
      },
    });
  }

  // No-oracle: 403/404 both map to a generic message (mirrors thread-view.ts).
  private describeLoad(e: HttpErrorResponse): string {
    if (e.status === 403 || e.status === 404) return "You don't have access to do that.";
    return "We couldn't load this contact.";
  }

  private describeMutation(e: HttpErrorResponse, fallback: string): string {
    if (e.status === 400) {
      const msg = (e.error as { message?: string } | null)?.message;
      return msg ? `That change was rejected: ${msg}` : 'That change was rejected. Check your input.';
    }
    if (e.status === 409) return 'That conflicts with the current state.';
    if (e.status === 403 || e.status === 404) return "You don't have access to do that.";
    return fallback;
  }
}
