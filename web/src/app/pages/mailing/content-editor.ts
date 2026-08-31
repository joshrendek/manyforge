import { Component, ElementRef, EventEmitter, Input, Output, ViewChild } from '@angular/core';
import { FormsModule } from '@angular/forms';

export interface MailingContentDraft {
  subject: string;
  preheader: string;
  body_markdown: string;
  track_opens: boolean;
  track_clicks: boolean;
}

@Component({
  selector: 'app-mailing-content-editor',
  standalone: true,
  imports: [FormsModule],
  template: `
    <div class="content-fields" data-testid="mailing-content-editor">
      <div class="mf-field">
        <label for="mailing-content-subject">Subject</label>
        <input
          id="mailing-content-subject"
          class="mf-input"
          data-testid="mailing-content-subject"
          name="contentSubject"
          [ngModel]="value.subject"
          [readonly]="readOnly"
          (ngModelChange)="change({ subject: $event })"
        />
      </div>
      <div class="mf-field">
        <label for="mailing-content-preheader">Preheader</label>
        <input
          id="mailing-content-preheader"
          class="mf-input"
          data-testid="mailing-content-preheader"
          name="contentPreheader"
          placeholder="Optional inbox preview text"
          [ngModel]="value.preheader"
          [readonly]="readOnly"
          (ngModelChange)="change({ preheader: $event })"
        />
      </div>
      <div class="mf-field">
        <span class="field-label">Variables</span>
        <div class="variables" data-testid="mailing-variable-palette">
          @for (variable of variables; track variable) {
            <button
              type="button"
              class="mf-btn mf-btn-ghost mf-btn-sm variable"
              [attr.data-testid]="'mailing-variable-' + variable"
              [disabled]="readOnly"
              (click)="insertVariable(variable)"
            >
              {{ variableToken(variable) }}
            </button>
          }
        </div>
      </div>
      <div class="mf-field">
        <label for="mailing-content-body">Markdown body</label>
        <textarea
          #body
          id="mailing-content-body"
          class="mf-textarea body"
          data-testid="mailing-content-body"
          name="contentBody"
          rows="20"
          [ngModel]="value.body_markdown"
          [readonly]="readOnly"
          (ngModelChange)="change({ body_markdown: $event })"
        ></textarea>
      </div>
      <div class="tracking">
        <label class="mf-check">
          <input
            type="checkbox"
            name="contentTrackOpens"
            data-testid="mailing-track-opens"
            [ngModel]="value.track_opens"
            [disabled]="readOnly"
            (ngModelChange)="change({ track_opens: $event })"
          />
          Track opens
        </label>
        <label class="mf-check">
          <input
            type="checkbox"
            name="contentTrackClicks"
            data-testid="mailing-track-clicks"
            [ngModel]="value.track_clicks"
            [disabled]="readOnly"
            (ngModelChange)="change({ track_clicks: $event })"
          />
          Track clicks
        </label>
      </div>
    </div>
  `,
  styles: [
    `
      .content-fields {
        display: grid;
        gap: 16px;
      }
      .field-label {
        display: block;
        margin-bottom: 6px;
        color: var(--mf-text-muted);
        font-size: var(--mf-fs-sm);
        font-weight: 500;
      }
      .variables,
      .tracking {
        display: flex;
        flex-wrap: wrap;
        align-items: center;
        gap: 8px;
      }
      .variable {
        font-family: var(--mf-mono, ui-monospace, monospace);
      }
      .body {
        min-height: 360px;
        resize: vertical;
        font-family: var(--mf-mono, ui-monospace, monospace);
        line-height: 1.55;
      }
      .mf-check {
        display: flex;
        align-items: center;
        gap: 7px;
        font-size: var(--mf-fs-sm);
      }
    `,
  ],
})
export class MailingContentEditorComponent {
  @Input({ required: true }) value!: MailingContentDraft;
  @Input() readOnly = false;
  @Output() valueChange = new EventEmitter<MailingContentDraft>();
  @ViewChild('body') private body?: ElementRef<HTMLTextAreaElement>;

  readonly variables = ['first_name', 'last_name', 'email', 'unsubscribe_url', 'list_name'];

  variableToken(name: string): string {
    return `{{${name}}}`;
  }

  change(patch: Partial<MailingContentDraft>): void {
    if (this.readOnly) return;
    this.valueChange.emit({ ...this.value, ...patch });
  }

  insertVariable(name: string): void {
    if (this.readOnly || !this.body) return;
    const textarea = this.body.nativeElement;
    const start = textarea.selectionStart ?? this.value.body_markdown.length;
    const end = textarea.selectionEnd ?? start;
    const token = `{{${name}}}`;
    const body =
      this.value.body_markdown.slice(0, start) + token + this.value.body_markdown.slice(end);
    textarea.value = body;
    this.change({ body_markdown: body });
    queueMicrotask(() => {
      textarea.focus();
      textarea.setSelectionRange(start + token.length, start + token.length);
    });
  }
}
