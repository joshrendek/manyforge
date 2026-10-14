import { Component, EventEmitter, Input, Output } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { LucideAngularModule, UserRound } from 'lucide-angular';
import { WorkStatus, WorkTask } from '../../core/work.service';
import { StatusPill } from '../../ui/status-pill/status-pill';
import {
  STATUSES,
  priorityLabel,
  priorityTitle,
  priorityTone,
  workStatusLabel,
} from './work-labels';

// A Board card. Operator-lane cards carry an accent rail + "Operator" tag so human asks stand out
// in a mixed column. Moving is a compact status select (no drag-and-drop); the title opens the
// detail dialog.
@Component({
  selector: 'app-work-task-card',
  imports: [FormsModule, LucideAngularModule, StatusPill],
  template: `
    <article
      class="task-card"
      data-testid="work-card"
      [attr.data-key]="task.key"
      [attr.data-lane]="task.lane"
      [class.is-operator]="task.lane === 'operator'"
      [class.is-closed]="task.status === 'done' || task.status === 'canceled'"
    >
      <div class="card-top">
        <code class="key">{{ task.key }}</code>
        <mf-status-pill
          [tone]="priorityTone(task.priority)"
          [label]="priorityLabel(task.priority)"
          [ariaLabel]="priorityTitle(task.priority)"
          [outline]="task.priority > 0"
        />
      </div>
      <button type="button" class="card-title" data-testid="work-task-open" (click)="opened.emit()">
        {{ task.title }}
      </button>
      <div class="card-tags">
        @if (task.lane === 'operator') {
          <span class="lane-tag" data-testid="work-card-operator"
            ><lucide-icon [img]="UserRound" [size]="12" aria-hidden="true" />Operator</span
          >
        }
        <span>{{ task.type }}</span>
        @if (task.assignee) {
          <span class="assignee" data-testid="work-card-assignee">{{ '@' + task.assignee }}</span>
        }
      </div>
      @if (task.blocked_by.length) {
        <p class="blockers" data-testid="work-card-blocked-by">
          Blocked by {{ task.blocked_by.join(', ') }}
        </p>
      }
      <select
        class="mf-select move"
        data-testid="work-card-status"
        [attr.aria-label]="'Move ' + task.key"
        [ngModel]="task.status"
        (ngModelChange)="moved.emit($event)"
        [disabled]="busy"
      >
        @for (s of statuses; track s) {
          <option [value]="s">{{ statusLabel(s) }}</option>
        }
      </select>
    </article>
  `,
  styles: [
    `
      .task-card {
        display: flex;
        flex-direction: column;
        gap: 6px;
        background: var(--mf-surface);
        border: 1px solid var(--mf-border);
        border-radius: var(--mf-radius-sm);
        padding: 10px 10px 10px 12px;
        box-shadow: var(--mf-shadow-sm);
      }
      .task-card.is-operator {
        background: var(--mf-accent-soft);
        border-color: var(--mf-accent);
        box-shadow: inset 3px 0 0 var(--mf-accent);
      }
      .task-card.is-closed {
        background: var(--mf-surface);
        opacity: 0.75;
      }
      .card-top {
        display: flex;
        align-items: center;
        justify-content: space-between;
        gap: 8px;
      }
      .key {
        font-family: var(--mf-font-mono);
        font-size: var(--mf-fs-xs);
        color: var(--mf-text-muted);
      }
      .card-title {
        all: unset;
        cursor: pointer;
        color: var(--mf-text);
        font-weight: var(--mf-fw-semibold);
        font-size: var(--mf-fs-sm);
        line-height: var(--mf-lh-snug);
        overflow-wrap: anywhere;
      }
      .card-title:hover {
        color: var(--mf-accent-text);
      }
      .card-title:focus-visible {
        border-radius: var(--mf-radius-xs);
        box-shadow: var(--mf-ring);
      }
      .card-tags {
        display: flex;
        align-items: center;
        flex-wrap: wrap;
        gap: 8px;
        font-size: var(--mf-fs-xs);
        color: var(--mf-text-faint);
      }
      .lane-tag {
        display: inline-flex;
        align-items: center;
        gap: 4px;
        font: var(--mf-text-eyebrow);
        letter-spacing: var(--mf-tracking-label);
        text-transform: uppercase;
        color: var(--mf-accent-text);
      }
      .assignee {
        font-family: var(--mf-font-mono);
        color: var(--mf-text-muted);
      }
      .blockers {
        margin: 0;
        font-size: var(--mf-fs-xs);
        color: var(--mf-danger-text);
      }
      .move {
        height: var(--mf-control-h-sm);
        font-size: var(--mf-fs-xs);
      }
    `,
  ],
})
export class WorkTaskCardComponent {
  @Input({ required: true }) task!: WorkTask;
  @Input() busy = false;
  @Output() opened = new EventEmitter<void>();
  @Output() moved = new EventEmitter<WorkStatus>();

  readonly UserRound = UserRound;
  readonly statuses = STATUSES;
  readonly statusLabel = workStatusLabel;
  readonly priorityLabel = priorityLabel;
  readonly priorityTitle = priorityTitle;
  readonly priorityTone = priorityTone;
}
