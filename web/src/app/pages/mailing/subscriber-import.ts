import { Component, EventEmitter, Input, Output, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { MailingImportResult, MailingService } from '../../core/mailing.service';
import { ToastService } from '../../ui/toast/toast.service';

@Component({
  selector: 'app-mailing-subscriber-import',
  standalone: true,
  imports: [FormsModule],
  template: `
    <section class="mf-card import-card" data-testid="subscriber-import">
      <h3>Import CSV</h3>
      <p class="hint">Upload up to 5 MiB. Include an <code>email</code> column.</p>
      <div class="mf-field">
        <label for="mailing-import-file">CSV file</label>
        <input
          id="mailing-import-file"
          type="file"
          accept=".csv,text/csv"
          data-testid="subscriber-import-file"
          (change)="selectFile($event)"
        />
      </div>
      <label class="mf-check" for="mailing-import-consent">
        <input
          id="mailing-import-consent"
          type="checkbox"
          data-testid="subscriber-import-consent"
          [(ngModel)]="consentAttested"
        />
        I attest that these people consented to receive email.
      </label>
      <label class="mf-check" for="mailing-import-confirmation">
        <input
          id="mailing-import-confirmation"
          type="checkbox"
          data-testid="subscriber-import-skip-confirmation"
          [(ngModel)]="skipConfirmation"
        />
        Add as active without confirmation
      </label>
      <button
        type="button"
        class="mf-btn mf-btn-primary mf-btn-sm"
        data-testid="subscriber-import-submit"
        [disabled]="!file || !consentAttested || importing()"
        (click)="submit()"
      >
        {{ importing() ? 'Importing…' : 'Import subscribers' }}
      </button>
      @if (result(); as r) {
        <p class="result" data-testid="subscriber-import-result">
          Imported {{ r.imported }}; skipped {{ r.skipped }}.
          @if (r.errors.length) {
            {{ r.errors.length }} row errors.
          }
        </p>
      }
    </section>
  `,
  styles: [
    `
      .import-card {
        display: grid;
        gap: 12px;
      }
      h3,
      p {
        margin: 0;
      }
      .hint,
      .result {
        color: var(--mf-text-muted);
        font-size: var(--mf-fs-sm);
      }
      .mf-check {
        display: flex;
        align-items: center;
        gap: 8px;
        font-size: var(--mf-fs-sm);
      }
    `,
  ],
})
export class SubscriberImportComponent {
  private mailing = inject(MailingService);
  private toast = inject(ToastService);

  @Input({ required: true }) businessId = '';
  @Input({ required: true }) listId = '';
  @Output() imported = new EventEmitter<MailingImportResult>();

  file: File | null = null;
  consentAttested = false;
  skipConfirmation = false;
  importing = signal(false);
  result = signal<MailingImportResult | null>(null);

  selectFile(event: Event): void {
    this.file = (event.target as HTMLInputElement).files?.[0] ?? null;
  }

  submit(): void {
    if (!this.file || !this.consentAttested || this.importing()) return;
    this.importing.set(true);
    this.mailing
      .importSubscribers(
        this.businessId,
        this.listId,
        this.file,
        this.consentAttested,
        this.skipConfirmation,
      )
      .subscribe({
        next: (result) => {
          this.result.set(result);
          this.importing.set(false);
          this.toast.success(`Imported ${result.imported} subscribers`);
          this.imported.emit(result);
        },
        error: () => {
          this.importing.set(false);
          this.toast.error('Could not import subscribers');
        },
      });
  }
}
