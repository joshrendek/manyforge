import {
  Component,
  Input,
  OnChanges,
  OnDestroy,
  SimpleChanges,
  inject,
  signal,
} from '@angular/core';
import { MailingPreview, MailingPreviewInput, MailingService } from '../../core/mailing.service';
import { MarkdownPreview } from '../../ui/markdown-preview/markdown-preview';
import { Spinner } from '../../ui/spinner/spinner';
import { MailingContentDraft } from './content-editor';

export type MailingPreviewKind = 'campaigns' | 'templates';

@Component({
  selector: 'app-mailing-preview-pane',
  standalone: true,
  imports: [MarkdownPreview, Spinner],
  template: `
    <section class="preview" data-testid="mailing-preview-pane">
      <div class="preview-head">
        <strong>Preview</strong>
        <div class="mode" role="group" aria-label="Preview format">
          <button
            type="button"
            class="mf-btn mf-btn-sm"
            data-testid="mailing-preview-html"
            [attr.aria-pressed]="mode() === 'html'"
            [class.mf-btn-primary]="mode() === 'html'"
            [class.mf-btn-ghost]="mode() !== 'html'"
            (click)="mode.set('html')"
          >
            HTML
          </button>
          <button
            type="button"
            class="mf-btn mf-btn-sm"
            data-testid="mailing-preview-text"
            [attr.aria-pressed]="mode() === 'text'"
            [class.mf-btn-primary]="mode() === 'text'"
            [class.mf-btn-ghost]="mode() !== 'text'"
            (click)="mode.set('text')"
          >
            Text
          </button>
        </div>
      </div>
      @if (loading()) {
        <div class="loading" data-testid="mailing-preview-loading"><mf-spinner /> Rendering…</div>
      }
      @if (mode() === 'html') {
        <mf-markdown-preview [html]="preview().html" />
      } @else {
        <pre data-testid="mailing-preview-text-output">{{ preview().text }}</pre>
      }
      @if (error()) {
        <p class="mf-err" data-testid="mailing-preview-error">{{ error() }}</p>
      }
    </section>
  `,
  styles: [
    `
      .preview {
        min-width: 0;
      }
      .preview-head,
      .mode,
      .loading {
        display: flex;
        align-items: center;
        gap: 8px;
      }
      .preview-head {
        justify-content: space-between;
        margin-bottom: 12px;
      }
      .loading {
        margin-bottom: 8px;
        color: var(--mf-text-muted);
        font-size: var(--mf-fs-sm);
      }
      pre {
        box-sizing: border-box;
        height: 70vh;
        margin: 0;
        overflow: auto;
        white-space: pre-wrap;
        padding: 16px;
        border: 1px solid var(--mf-border);
        border-radius: var(--mf-radius-sm);
        background: var(--mf-surface-inset);
        color: var(--mf-text);
        font: var(--mf-fs-base) / 1.55 var(--mf-mono, ui-monospace, monospace);
      }
    `,
  ],
})
export class MailingPreviewPaneComponent implements OnChanges, OnDestroy {
  private mailing = inject(MailingService);
  private timer: ReturnType<typeof setTimeout> | undefined;
  private previewSeq = 0;

  @Input({ required: true }) businessId = '';
  @Input({ required: true }) kind: MailingPreviewKind = 'campaigns';
  @Input({ required: true }) content!: MailingContentDraft;
  @Input() fromName: string | null = null;
  @Input() postalAddress: string | null = null;

  mode = signal<'html' | 'text'>('html');
  preview = signal<MailingPreview>({ html: '', text: '' });
  loading = signal(false);
  error = signal('');

  ngOnChanges(_changes: SimpleChanges): void {
    this.schedulePreview();
  }

  ngOnDestroy(): void {
    this.previewSeq++;
    if (this.timer) clearTimeout(this.timer);
  }

  private schedulePreview(): void {
    const seq = ++this.previewSeq;
    if (this.timer) clearTimeout(this.timer);
    if (!this.businessId || !this.content) return;
    this.timer = setTimeout(() => this.loadPreview(seq), 400);
  }

  private loadPreview(seq: number): void {
    const body: MailingPreviewInput = {
      body_markdown: this.content.body_markdown,
      preheader: this.content.preheader.trim() || null,
      from_name: this.fromName,
      postal_address: this.postalAddress,
    };
    this.loading.set(true);
    this.error.set('');
    const request =
      this.kind === 'templates'
        ? this.mailing.previewTemplate(this.businessId, body)
        : this.mailing.previewCampaign(this.businessId, body);
    request.subscribe({
      next: (preview) => {
        if (seq !== this.previewSeq) return;
        this.preview.set(preview);
        this.loading.set(false);
      },
      error: () => {
        if (seq !== this.previewSeq) return;
        this.loading.set(false);
        this.error.set('Could not render preview');
      },
    });
  }
}
