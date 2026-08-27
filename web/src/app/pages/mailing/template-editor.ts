import { Component, OnInit, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { ActivatedRoute, Router, RouterLink } from '@angular/router';
import { MailingService, MailingTemplate } from '../../core/mailing.service';
import { PageHeader } from '../../ui/page-header/page-header';
import { Spinner } from '../../ui/spinner/spinner';
import { ToastService } from '../../ui/toast/toast.service';

@Component({
  selector: 'app-mailing-template-editor',
  standalone: true,
  imports: [FormsModule, RouterLink, PageHeader, Spinner],
  template: `
    <div class="mf-card" data-testid="mailing-template-editor">
      <mf-page-header
        title="Template editor"
        subtitle="Markdown content is rendered when a campaign is sent"
      >
        <a
          routerLink="/mailing/templates"
          class="mf-btn mf-btn-ghost mf-btn-sm"
          data-testid="template-editor-back"
          actions
          >Back to templates</a
        >
      </mf-page-header>
      @if (loading()) {
        <div class="loading" data-testid="template-editor-loading">
          <mf-spinner /> Loading template…
        </div>
      } @else if (template()) {
        <form class="editor" data-testid="template-editor-form" (ngSubmit)="save()">
          <div class="top-fields">
            <div class="mf-field">
              <label for="editor-name">Name</label
              ><input
                id="editor-name"
                class="mf-input"
                name="name"
                data-testid="template-editor-name"
                [(ngModel)]="name"
                required
              />
            </div>
            <div class="mf-field">
              <label for="editor-subject">Subject</label
              ><input
                id="editor-subject"
                class="mf-input"
                name="subject"
                data-testid="template-editor-subject"
                [(ngModel)]="subject"
                required
              />
            </div>
          </div>
          <div class="mf-field">
            <label for="editor-preheader">Preheader</label
            ><input
              id="editor-preheader"
              class="mf-input"
              name="preheader"
              data-testid="template-editor-preheader"
              [(ngModel)]="preheader"
              placeholder="Optional inbox preview text"
            />
          </div>
          <div class="mf-field">
            <label for="editor-body">Markdown body</label
            ><textarea
              id="editor-body"
              class="mf-input body"
              name="body"
              data-testid="template-editor-body"
              [(ngModel)]="bodyMarkdown"
              rows="20"
            ></textarea>
          </div>
          <div class="tracking">
            <label class="mf-check"
              ><input
                type="checkbox"
                name="opens"
                data-testid="template-track-opens"
                [(ngModel)]="trackOpens"
              />
              Track opens</label
            >
            <label class="mf-check"
              ><input
                type="checkbox"
                name="clicks"
                data-testid="template-track-clicks"
                [(ngModel)]="trackClicks"
              />
              Track clicks</label
            >
          </div>
          <div class="actions">
            <button
              type="submit"
              class="mf-btn mf-btn-primary"
              data-testid="template-editor-save"
              [disabled]="!name.trim() || !subject.trim() || saving()"
            >
              {{ saving() ? 'Saving…' : 'Save template' }}
            </button>
            @if (!confirmDelete()) {
              <button
                type="button"
                class="mf-btn mf-btn-danger"
                data-testid="template-editor-delete"
                (click)="confirmDelete.set(true)"
              >
                Delete
              </button>
            } @else {
              <button
                type="button"
                class="mf-btn mf-btn-danger"
                data-testid="template-editor-delete-confirm"
                [disabled]="deleting()"
                (click)="remove()"
              >
                {{ deleting() ? 'Deleting…' : 'Confirm delete' }}
              </button>
              <button
                type="button"
                class="mf-btn mf-btn-ghost"
                data-testid="template-editor-delete-cancel"
                (click)="confirmDelete.set(false)"
              >
                Cancel
              </button>
            }
          </div>
        </form>
      }
      @if (error()) {
        <p class="mf-err" data-testid="template-editor-error">{{ error() }}</p>
      }
    </div>
  `,
  styles: [
    `
      .loading,
      .tracking,
      .actions {
        display: flex;
        align-items: center;
        gap: 12px;
      }
      .editor {
        display: grid;
        gap: 16px;
      }
      .top-fields {
        display: grid;
        grid-template-columns: repeat(2, minmax(0, 1fr));
        gap: 16px;
      }
      .body {
        resize: vertical;
        min-height: 360px;
        font-family: var(--mf-mono, ui-monospace, monospace);
        line-height: 1.55;
      }
      .mf-check {
        display: flex;
        align-items: center;
        gap: 7px;
        font-size: var(--mf-fs-sm);
      }
      @media (max-width: 760px) {
        .top-fields {
          grid-template-columns: 1fr;
        }
      }
    `,
  ],
})
export class MailingTemplateEditorComponent implements OnInit {
  private route = inject(ActivatedRoute);
  private router = inject(Router);
  private mailing = inject(MailingService);
  private toast = inject(ToastService);

  businessId = '';
  templateId = '';
  template = signal<MailingTemplate | null>(null);
  loading = signal(true);
  saving = signal(false);
  deleting = signal(false);
  confirmDelete = signal(false);
  error = signal('');
  name = '';
  subject = '';
  preheader = '';
  bodyMarkdown = '';
  trackOpens = true;
  trackClicks = true;

  ngOnInit(): void {
    this.businessId = this.route.snapshot.paramMap.get('businessId') ?? '';
    this.templateId = this.route.snapshot.paramMap.get('templateId') ?? '';
    if (!this.businessId || !this.templateId) {
      this.loading.set(false);
      this.error.set('Template route is invalid');
      return;
    }
    this.mailing.getTemplate(this.businessId, this.templateId).subscribe({
      next: (template) => {
        this.populate(template);
        this.loading.set(false);
      },
      error: () => {
        this.loading.set(false);
        this.error.set('Could not load template');
      },
    });
  }

  private populate(template: MailingTemplate): void {
    this.template.set(template);
    this.name = template.name;
    this.subject = template.subject;
    this.preheader = template.preheader ?? '';
    this.bodyMarkdown = template.body_markdown;
    this.trackOpens = template.track_opens;
    this.trackClicks = template.track_clicks;
  }

  save(): void {
    if (!this.name.trim() || !this.subject.trim() || this.saving()) return;
    this.saving.set(true);
    this.mailing
      .updateTemplate(this.businessId, this.templateId, {
        name: this.name.trim(),
        subject: this.subject.trim(),
        preheader: this.preheader.trim() || null,
        body_markdown: this.bodyMarkdown,
        track_opens: this.trackOpens,
        track_clicks: this.trackClicks,
      })
      .subscribe({
        next: (template) => {
          this.populate(template);
          this.saving.set(false);
          this.toast.success('Template saved');
        },
        error: () => {
          this.saving.set(false);
          this.toast.error('Could not save template');
        },
      });
  }

  remove(): void {
    if (this.deleting()) return;
    this.deleting.set(true);
    this.mailing.deleteTemplate(this.businessId, this.templateId).subscribe({
      next: () => {
        this.toast.success('Template deleted');
        void this.router.navigate(['/mailing/templates']);
      },
      error: () => {
        this.deleting.set(false);
        this.toast.error('Could not delete template');
      },
    });
  }
}
