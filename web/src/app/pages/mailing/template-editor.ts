import { Component, HostListener, OnInit, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { ActivatedRoute, Router, RouterLink } from '@angular/router';
import { MailingService, MailingTemplate } from '../../core/mailing.service';
import { HasUnsavedChanges, protectBeforeUnload } from '../../core/unsaved-changes.guard';
import { PageHeader } from '../../ui/page-header/page-header';
import { Spinner } from '../../ui/spinner/spinner';
import { ToastService } from '../../ui/toast/toast.service';
import { MailingContentDraft, MailingContentEditorComponent } from './content-editor';
import { MailingPreviewPaneComponent } from './preview-pane';

@Component({
  selector: 'app-mailing-template-editor',
  standalone: true,
  imports: [
    FormsModule,
    RouterLink,
    PageHeader,
    Spinner,
    MailingContentEditorComponent,
    MailingPreviewPaneComponent,
  ],
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
        <div class="workspace" data-testid="template-editor-form">
          <section class="form-pane">
            <div class="mf-field">
              <label for="editor-name">Name</label>
              <input
                id="editor-name"
                class="mf-input"
                name="name"
                data-testid="template-editor-name"
                [(ngModel)]="name"
                [readonly]="saving() || deleting()"
                required
              />
            </div>
            <app-mailing-content-editor
              [value]="content()"
              [readOnly]="saving() || deleting()"
              (valueChange)="content.set($event)"
            />
          </section>
          <app-mailing-preview-pane
            [businessId]="businessId"
            kind="templates"
            [content]="content()"
          />
        </div>

        <div class="actions">
          <button
            type="button"
            class="mf-btn mf-btn-primary"
            data-testid="template-editor-save"
            [disabled]="!canSave()"
            (click)="save()"
          >
            {{ saving() ? 'Saving…' : 'Save template' }}
          </button>
          @if (!confirmDelete()) {
            <button
              type="button"
              class="mf-btn mf-btn-danger"
              data-testid="template-editor-delete"
              [disabled]="saving()"
              (click)="confirmDelete.set(true)"
            >
              Delete
            </button>
          } @else {
            <button
              type="button"
              class="mf-btn mf-btn-danger"
              data-testid="template-editor-delete-confirm"
              [disabled]="deleting() || saving()"
              (click)="remove()"
            >
              {{ deleting() ? 'Deleting…' : 'Confirm delete' }}
            </button>
            <button
              type="button"
              class="mf-btn mf-btn-ghost"
              data-testid="template-editor-delete-cancel"
              [disabled]="deleting() || saving()"
              (click)="confirmDelete.set(false)"
            >
              Cancel
            </button>
          }
        </div>
      }
      @if (error()) {
        <p class="mf-err" data-testid="template-editor-error">{{ error() }}</p>
      }
    </div>
  `,
  styles: [
    `
      .loading,
      .actions {
        display: flex;
        align-items: center;
        gap: 12px;
      }
      .workspace {
        display: grid;
        grid-template-columns: minmax(0, 1fr) minmax(0, 1fr);
        gap: 24px;
      }
      .form-pane {
        display: grid;
        align-content: start;
        gap: 16px;
        min-width: 0;
      }
      .actions {
        flex-wrap: wrap;
        margin-top: 20px;
        padding-top: 16px;
        border-top: 1px solid var(--mf-border);
      }
      @media (max-width: 960px) {
        .workspace {
          grid-template-columns: 1fr;
        }
      }
    `,
  ],
})
export class MailingTemplateEditorComponent implements OnInit, HasUnsavedChanges {
  private route = inject(ActivatedRoute);
  private router = inject(Router);
  private mailing = inject(MailingService);
  private toast = inject(ToastService);
  private savedSnapshot = '';

  businessId = '';
  templateId = '';
  template = signal<MailingTemplate | null>(null);
  content = signal<MailingContentDraft>({
    subject: '',
    preheader: '',
    body_markdown: '',
    track_opens: true,
    track_clicks: true,
  });
  loading = signal(true);
  saving = signal(false);
  deleting = signal(false);
  confirmDelete = signal(false);
  error = signal('');
  name = '';

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

  canSave(): boolean {
    return (
      !!this.name.trim() &&
      !!this.content().subject.trim() &&
      !this.saving() &&
      !this.deleting() &&
      this.hasUnsavedChanges()
    );
  }

  hasUnsavedChanges(): boolean {
    return !!this.savedSnapshot && this.snapshot() !== this.savedSnapshot;
  }

  @HostListener('window:beforeunload', ['$event'])
  beforeUnload(event: BeforeUnloadEvent): void {
    protectBeforeUnload(event, this.hasUnsavedChanges());
  }

  save(): void {
    if (!this.name.trim() || !this.content().subject.trim() || this.saving() || this.deleting())
      return;
    this.saving.set(true);
    const content = this.content();
    this.mailing
      .updateTemplate(this.businessId, this.templateId, {
        name: this.name.trim(),
        subject: content.subject.trim(),
        preheader: content.preheader.trim() || null,
        body_markdown: content.body_markdown,
        track_opens: content.track_opens,
        track_clicks: content.track_clicks,
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
    if (this.deleting() || this.saving()) return;
    this.deleting.set(true);
    this.mailing.deleteTemplate(this.businessId, this.templateId).subscribe({
      next: () => {
        this.savedSnapshot = this.snapshot();
        this.toast.success('Template deleted');
        void this.router.navigate(['/mailing/templates']);
      },
      error: () => {
        this.deleting.set(false);
        this.toast.error('Could not delete template');
      },
    });
  }

  private populate(template: MailingTemplate): void {
    this.template.set(template);
    this.name = template.name;
    this.content.set({
      subject: template.subject,
      preheader: template.preheader ?? '',
      body_markdown: template.body_markdown,
      track_opens: template.track_opens,
      track_clicks: template.track_clicks,
    });
    this.savedSnapshot = this.snapshot();
  }

  private snapshot(): string {
    return JSON.stringify({ name: this.name, content: this.content() });
  }
}
