import {
  Component,
  EventEmitter,
  Input,
  OnChanges,
  Output,
  SimpleChanges,
  inject,
  signal,
} from '@angular/core';
import { FormsModule } from '@angular/forms';
import { Contact, CrmService } from '../../core/crm.service';
import { MailingImportResult, MailingService } from '../../core/mailing.service';
import { Spinner } from '../../ui/spinner/spinner';
import { ToastService } from '../../ui/toast/toast.service';

@Component({
  selector: 'app-mailing-contacts-picker',
  standalone: true,
  imports: [FormsModule, Spinner],
  template: `
    <section class="mf-card picker" data-testid="contacts-picker">
      <div class="title-row">
        <div>
          <h3>Add from contacts</h3>
          <p>Select CRM contacts to add to this list.</p>
        </div>
        @if (loading()) {
          <mf-spinner />
        }
      </div>
      <div class="contact-list" data-testid="contacts-picker-list">
        @for (contact of contacts(); track contact.id) {
          <label class="contact-row" data-testid="contacts-picker-row">
            <input
              type="checkbox"
              data-testid="contacts-picker-checkbox"
              [checked]="selected.has(contact.id)"
              (change)="toggle(contact.id, $event)"
            />
            <span>{{ contact.display_name || contact.primary_email }}</span>
            <span class="email">{{ contact.primary_email }}</span>
          </label>
        }
        @if (!contacts().length && !loading()) {
          <p data-testid="contacts-picker-empty">No contacts found.</p>
        }
      </div>
      @if (nextCursor()) {
        <button
          type="button"
          class="mf-btn mf-btn-ghost mf-btn-sm"
          data-testid="contacts-picker-load-more"
          [disabled]="loading()"
          (click)="loadMore()"
        >
          Load more
        </button>
      }
      <label class="mf-check">
        <input
          type="checkbox"
          data-testid="contacts-picker-skip-confirmation"
          [(ngModel)]="skipConfirmation"
        />
        Add as active without confirmation
      </label>
      <button
        type="button"
        class="mf-btn mf-btn-primary mf-btn-sm"
        data-testid="contacts-picker-submit"
        [disabled]="!selected.size || adding()"
        (click)="addSelected()"
      >
        {{ adding() ? 'Adding…' : 'Add ' + selected.size + ' selected' }}
      </button>
    </section>
  `,
  styles: [
    `
      .picker {
        display: grid;
        gap: 12px;
      }
      .title-row,
      .contact-row {
        display: flex;
        align-items: center;
        gap: 10px;
      }
      .title-row {
        justify-content: space-between;
      }
      h3,
      p {
        margin: 0;
      }
      .title-row p,
      .email,
      .contact-list > p {
        color: var(--mf-text-muted);
        font-size: var(--mf-fs-sm);
      }
      .contact-list {
        display: grid;
        max-height: 260px;
        overflow: auto;
        border-top: 1px solid var(--mf-border);
      }
      .contact-row {
        padding: 9px 2px;
        border-bottom: 1px solid var(--mf-border);
      }
      .contact-row > span:first-of-type {
        flex: 1;
      }
      .mf-check {
        display: flex;
        align-items: center;
        gap: 8px;
        font-size: var(--mf-fs-sm);
      }
      .mf-btn {
        justify-self: start;
      }
    `,
  ],
})
export class ContactsPickerComponent implements OnChanges {
  private crm = inject(CrmService);
  private mailing = inject(MailingService);
  private toast = inject(ToastService);

  @Input({ required: true }) businessId = '';
  @Input({ required: true }) listId = '';
  @Output() added = new EventEmitter<MailingImportResult>();

  contacts = signal<Contact[]>([]);
  nextCursor = signal<string | null>(null);
  loading = signal(false);
  adding = signal(false);
  selected = new Set<string>();
  skipConfirmation = false;

  ngOnChanges(changes: SimpleChanges): void {
    if (changes['businessId'] && this.businessId) this.reload();
  }

  reload(): void {
    this.contacts.set([]);
    this.nextCursor.set(null);
    this.selected = new Set<string>();
    this.load();
  }

  loadMore(): void {
    if (this.nextCursor()) this.load(this.nextCursor()!);
  }

  private load(cursor?: string): void {
    if (!this.businessId || this.loading()) return;
    this.loading.set(true);
    this.crm.listContacts(this.businessId, cursor).subscribe({
      next: (page) => {
        this.contacts.update((items) =>
          cursor ? [...items, ...(page.items ?? [])] : (page.items ?? []),
        );
        this.nextCursor.set(page.next_cursor ?? null);
        this.loading.set(false);
      },
      error: () => {
        this.loading.set(false);
        this.toast.error('Could not load contacts');
      },
    });
  }

  toggle(id: string, event: Event): void {
    const next = new Set(this.selected);
    if ((event.target as HTMLInputElement).checked) next.add(id);
    else next.delete(id);
    this.selected = next;
  }

  addSelected(): void {
    if (!this.selected.size || this.adding()) return;
    this.adding.set(true);
    this.mailing
      .addFromContacts(this.businessId, this.listId, [...this.selected], this.skipConfirmation)
      .subscribe({
        next: (result) => {
          this.adding.set(false);
          this.selected = new Set<string>();
          this.toast.success(`Added ${result.imported} contacts`);
          this.added.emit(result);
        },
        error: () => {
          this.adding.set(false);
          this.toast.error('Could not add contacts');
        },
      });
  }
}
