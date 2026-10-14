import { DatePipe } from '@angular/common';
import { Component, EventEmitter, Input, Output } from '@angular/core';
import { WorkTask, WorkTaskPatch } from '../../core/work.service';
import { EmptyState } from '../../ui/empty-state/empty-state';
import { StatusPill } from '../../ui/status-pill/status-pill';
import { priorityLabel, priorityTitle, priorityTone, workStatusLabel } from './work-labels';

export interface WorkTaskChange {
  task: WorkTask;
  patch: WorkTaskPatch;
  ok?: string;
}

// The Operator inbox: open operator-lane tasks, highest priority first, with the agent's
// instructions (the description) shown in full so the human can act without opening anything.
// "Mark done" is the primary action; the agent sees it on its next `mf sync`.
@Component({
  selector: 'app-work-operator-inbox',
  imports: [DatePipe, EmptyState, StatusPill],
  template: `
    @if (tasks.length) {
      <p class="inbox-lede" data-testid="work-operator-lede">
        <b>{{ tasks.length }}</b>
        {{ tasks.length === 1 ? 'thing needs' : 'things need' }} a person. Agents are waiting on
        these — highest priority first.
      </p>
      <ol class="inbox" data-testid="work-operator-list">
        @for (t of tasks; track t.key) {
          <li
            class="ask"
            data-testid="work-operator-item"
            [attr.data-key]="t.key"
            [class.is-urgent]="t.priority <= 1"
          >
            <div class="ask-meta">
              <mf-status-pill
                [tone]="priorityTone(t.priority)"
                [label]="priorityLabel(t.priority)"
                [ariaLabel]="priorityTitle(t.priority)"
                [outline]="t.priority > 0"
              />
              <code class="key">{{ t.key }}</code>
              <span class="dot-sep">·</span>
              <span>{{ statusLabel(t.status) }}</span>
              <span class="dot-sep">·</span>
              <span>filed {{ t.created_at | date: 'MMM d, HH:mm' }}</span>
              @if (t.assignee) {
                <span class="dot-sep">·</span>
                <span>{{ t.assignee }}</span>
              }
            </div>
            <h3 class="ask-title">
              <button
                type="button"
                class="title-link"
                data-testid="work-task-open"
                (click)="opened.emit(t)"
              >
                {{ t.title }}
              </button>
            </h3>
            @if (t.description.trim()) {
              <div
                class="ask-body"
                data-testid="work-operator-description"
                [textContent]="t.description.trim()"
              ></div>
            }
            @if (t.blocked_by.length) {
              <p class="blockers">Waiting on {{ t.blocked_by.join(', ') }}</p>
            }
            <div class="ask-actions">
              <button
                type="button"
                class="mf-btn mf-btn-primary mf-btn-sm"
                data-testid="work-operator-done"
                [disabled]="busy.has(t.key)"
                (click)="
                  changed.emit({
                    task: t,
                    patch: { status: 'done' },
                    ok: 'Marked done — the agent will pick it up',
                  })
                "
              >
                Mark done
              </button>
              @if (t.status !== 'in_progress') {
                <button
                  type="button"
                  class="mf-btn mf-btn-ghost mf-btn-sm"
                  data-testid="work-operator-start"
                  [disabled]="busy.has(t.key)"
                  (click)="changed.emit({ task: t, patch: { status: 'in_progress' } })"
                >
                  I'm on it
                </button>
              }
              <button type="button" class="mf-btn mf-btn-ghost mf-btn-sm" (click)="opened.emit(t)">
                Details &amp; notes
              </button>
            </div>
          </li>
        }
      </ol>
    } @else {
      <mf-empty-state title="Nothing needs you right now" data-testid="work-operator-empty">
        When an agent is blocked on a person — rotate a credential, approve DNS, click install — it
        files an operator task and it shows up here.
      </mf-empty-state>
    }

    @if (done.length) {
      <div class="mf-section">Recently done</div>
      <div class="mf-table" data-testid="work-operator-done-list">
        @for (t of done; track t.key) {
          <div class="mf-tr done-row" data-testid="work-operator-done-row">
            <code style="flex:0 0 auto" class="key">{{ t.key }}</code>
            <span style="flex:1 1 140px;min-width:0" class="done-title">{{ t.title }}</span>
            <span style="flex:0 0 auto" class="when done-when">{{
              t.closed_at | date: 'MMM d, HH:mm'
            }}</span>
            <button
              type="button"
              class="mf-btn mf-btn-ghost mf-btn-sm"
              data-testid="work-operator-reopen"
              [disabled]="busy.has(t.key)"
              (click)="changed.emit({ task: t, patch: { status: 'todo' }, ok: 'Reopened' })"
            >
              Reopen
            </button>
          </div>
        }
      </div>
    }
  `,
  styles: [
    `
      .inbox-lede {
        margin: 0 0 12px;
        color: var(--mf-text-muted);
      }
      .inbox-lede b {
        color: var(--mf-accent-text);
      }
      .inbox {
        list-style: none;
        margin: 0;
        padding: 0;
        display: flex;
        flex-direction: column;
        gap: 10px;
      }
      .ask {
        border: 1px solid var(--mf-border);
        border-radius: var(--mf-radius-sm);
        background: var(--mf-surface);
        padding: 14px 16px 14px 18px;
        display: flex;
        flex-direction: column;
        gap: 8px;
      }
      .ask.is-urgent {
        border-color: var(--mf-accent);
        background: var(--mf-accent-soft);
      }
      .ask-meta {
        display: flex;
        align-items: center;
        flex-wrap: wrap;
        gap: 6px;
        font-size: var(--mf-fs-xs);
        color: var(--mf-text-muted);
      }
      .dot-sep {
        color: var(--mf-text-faint);
      }
      .ask-title {
        margin: 0;
        font: var(--mf-text-h2);
      }
      .ask-title .title-link {
        font-weight: var(--mf-fw-semibold);
      }
      .ask-body {
        white-space: pre-wrap;
        overflow-wrap: anywhere;
        font-size: var(--mf-fs-base);
        line-height: var(--mf-lh-base);
        color: var(--mf-text);
        background: var(--mf-surface-inset);
        border: 1px solid var(--mf-border);
        border-radius: var(--mf-radius-sm);
        padding: 10px 12px;
      }
      .ask-actions {
        display: flex;
        gap: 8px;
        flex-wrap: wrap;
      }
      .done-row {
        flex-wrap: wrap;
        row-gap: 4px;
        padding-top: 6px;
        padding-bottom: 6px;
      }
      @media (max-width: 640px) {
        .done-when {
          display: none;
        }
      }
      .done-row .done-title {
        color: var(--mf-text-muted);
        text-decoration: line-through;
        overflow: hidden;
        text-overflow: ellipsis;
        white-space: nowrap;
      }
      .key {
        font-family: var(--mf-font-mono);
        font-size: var(--mf-fs-xs);
        color: var(--mf-text-muted);
      }
      .title-link {
        all: unset;
        cursor: pointer;
        overflow-wrap: anywhere;
      }
      .title-link:hover {
        color: var(--mf-accent-text);
      }
      .title-link:focus-visible {
        border-radius: var(--mf-radius-xs);
        box-shadow: var(--mf-ring);
      }
      .blockers {
        margin: 0;
        font-size: var(--mf-fs-xs);
        color: var(--mf-danger-text);
      }
      .when {
        font-size: var(--mf-fs-sm);
        color: var(--mf-text-faint);
      }
    `,
  ],
})
export class WorkOperatorInboxComponent {
  @Input({ required: true }) tasks: WorkTask[] = [];
  @Input() done: WorkTask[] = [];
  @Input() busy: ReadonlySet<string> = new Set();
  @Output() opened = new EventEmitter<WorkTask>();
  @Output() changed = new EventEmitter<WorkTaskChange>();

  readonly statusLabel = workStatusLabel;
  readonly priorityLabel = priorityLabel;
  readonly priorityTitle = priorityTitle;
  readonly priorityTone = priorityTone;
}
