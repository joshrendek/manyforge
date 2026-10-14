import { DatePipe } from '@angular/common';
import { HttpErrorResponse } from '@angular/common/http';
import {
  AfterViewInit,
  Component,
  ElementRef,
  EventEmitter,
  Input,
  OnChanges,
  OnDestroy,
  Output,
  SimpleChanges,
  inject,
  signal,
  viewChild,
} from '@angular/core';
import { FormsModule } from '@angular/forms';
import { LucideAngularModule, UserRound, X } from 'lucide-angular';
import {
  WorkLane,
  WorkService,
  WorkStatus,
  WorkTask,
  WorkTaskPatch,
  WorkType,
} from '../../core/work.service';
import { StatusPill } from '../../ui/status-pill/status-pill';
import { TagChipInput } from '../../ui/tag-chip-input/tag-chip-input';
import { ToastService } from '../../ui/toast/toast.service';
import {
  LANES,
  describeWorkMutation,
  PRIORITIES,
  STATUSES,
  TYPES,
  isOpen,
  laneLabel,
  priorityTitle,
  sameList,
  trapTab,
  workStatusLabel,
  workStatusTone,
} from './work-labels';

// Task detail dialog. Holds an editable draft of the task; Save PATCHes only the fields that
// changed (so a concurrent agent sync to another field is not clobbered). Notes are append-only:
// "Add note" PATCHes { note } and the server stamps author + time. Every successful write emits
// the server's task so the board can patch its list.
@Component({
  selector: 'app-work-task-detail',
  host: { '(document:keydown.escape)': 'onEscape()' },
  imports: [FormsModule, DatePipe, LucideAngularModule, StatusPill, TagChipInput],
  template: `
    <div
      class="dialog-backdrop"
      data-testid="work-task-dialog"
      role="dialog"
      aria-modal="true"
      aria-labelledby="work-task-dialog-title"
      (click)="close()"
      (keydown)="onKeydown($event)"
    >
      <div
        class="mf-card dialog"
        #card
        tabindex="-1"
        [class.is-operator]="task.lane === 'operator'"
        (click)="$event.stopPropagation()"
      >
        <header class="dialog-head">
          <div class="dialog-meta">
            <code class="task-key" data-testid="work-task-dialog-key">{{ task.key }}</code>
            <mf-status-pill [tone]="statusTone(task.status)" [label]="statusLabel(task.status)" />
            @if (task.lane === 'operator') {
              <span class="lane-tag"
                ><lucide-icon [img]="UserRound" [size]="12" aria-hidden="true" />Operator</span
              >
            }
          </div>
          <button
            type="button"
            class="mf-btn mf-btn-ghost mf-btn-icon mf-btn-sm"
            aria-label="Close"
            data-testid="work-task-dialog-close"
            (click)="close()"
          >
            <lucide-icon [img]="X" [size]="15" aria-hidden="true" />
          </button>
        </header>
        <h2 id="work-task-dialog-title" class="sr-only">Task {{ task.key }}</h2>

        <form class="fields" (ngSubmit)="save()">
          <div class="mf-field">
            <label for="wt-title">Title</label>
            <input
              #titleInput
              id="wt-title"
              class="mf-input"
              name="title"
              data-testid="work-task-title"
              [(ngModel)]="draft.title"
            />
          </div>
          <div class="mf-field">
            <label for="wt-desc">Description</label>
            <textarea
              id="wt-desc"
              class="mf-textarea"
              name="description"
              rows="5"
              data-testid="work-task-description"
              [(ngModel)]="draft.description"
            ></textarea>
          </div>
          <div class="grid">
            <div class="mf-field">
              <label for="wt-status">Status</label>
              <select
                id="wt-status"
                class="mf-select"
                name="status"
                data-testid="work-task-status"
                [(ngModel)]="draft.status"
              >
                @for (s of statuses; track s) {
                  <option [value]="s">{{ statusLabel(s) }}</option>
                }
              </select>
            </div>
            <div class="mf-field">
              <label for="wt-lane">Lane</label>
              <select
                id="wt-lane"
                class="mf-select"
                name="lane"
                data-testid="work-task-lane"
                [(ngModel)]="draft.lane"
              >
                @for (l of lanes; track l) {
                  <option [value]="l">{{ laneLabel(l) }}</option>
                }
              </select>
            </div>
            <div class="mf-field">
              <label for="wt-priority">Priority</label>
              <select
                id="wt-priority"
                class="mf-select"
                name="priority"
                data-testid="work-task-priority"
                [(ngModel)]="draft.priority"
              >
                @for (p of priorities; track p) {
                  <option [ngValue]="p">{{ priorityTitle(p) }}</option>
                }
              </select>
            </div>
            <div class="mf-field">
              <label for="wt-type">Type</label>
              <select
                id="wt-type"
                class="mf-select"
                name="type"
                data-testid="work-task-type"
                [(ngModel)]="draft.type"
              >
                @for (t of types; track t) {
                  <option [value]="t">{{ t }}</option>
                }
              </select>
            </div>
          </div>
          <div class="mf-field">
            <label for="wt-assignee">Assignee</label>
            <input
              id="wt-assignee"
              class="mf-input"
              name="assignee"
              placeholder="agent name or handle"
              data-testid="work-task-assignee"
              [(ngModel)]="draft.assignee"
            />
          </div>
          <div class="mf-field">
            <span class="mf-label">Labels</span>
            <mf-tag-chip-input
              [tags]="draft.labels"
              placeholder="add label…"
              inputTestId="work-task-labels-input"
              chipTestId="work-task-label"
              (tagsChange)="draft.labels = $event"
            />
          </div>
          <div class="mf-field">
            <span class="mf-label">Blocked by</span>
            <mf-tag-chip-input
              [tags]="draft.blocked_by"
              placeholder="task key, e.g. mf-k3x9q2"
              inputTestId="work-task-blocked-input"
              chipTestId="work-task-blocker"
              (tagsChange)="draft.blocked_by = $event"
            />
          </div>
          <div class="actions">
            @if (isOpen(task)) {
              <button
                type="button"
                class="mf-btn mf-btn-ghost mf-btn-sm"
                data-testid="work-task-mark-done"
                [disabled]="busy()"
                (click)="markDone()"
              >
                Mark done
              </button>
            }
            <span class="spacer"></span>
            <button type="button" class="mf-btn mf-btn-ghost mf-btn-sm" (click)="close()">
              Close
            </button>
            <button
              type="submit"
              class="mf-btn mf-btn-primary mf-btn-sm"
              data-testid="work-task-save"
              [disabled]="!dirty() || !draft.title.trim() || busy()"
            >
              {{ busy() === 'save' ? 'Saving…' : 'Save changes' }}
            </button>
          </div>
        </form>

        <section class="notes" aria-label="Notes">
          <div class="mf-section">Notes</div>
          @if (task.notes.trim()) {
            <pre class="notes-body" data-testid="work-task-notes">{{ task.notes.trim() }}</pre>
          } @else {
            <p class="mf-text-muted none">No notes yet.</p>
          }
          <div class="mf-field">
            <label for="wt-note">Add note</label>
            <textarea
              #noteInput
              id="wt-note"
              class="mf-textarea"
              rows="2"
              placeholder="What happened, what's next…"
              data-testid="work-task-note-input"
              [(ngModel)]="noteText"
              [ngModelOptions]="{ standalone: true }"
            ></textarea>
          </div>
          <div>
            <button
              type="button"
              class="mf-btn mf-btn-sm"
              data-testid="work-task-add-note"
              [disabled]="!noteText.trim() || busy()"
              (click)="addNote()"
            >
              {{ busy() === 'note' ? 'Adding…' : 'Add note' }}
            </button>
          </div>
          <p class="stamps">
            Created {{ task.created_at | date: 'MMM d, HH:mm' }} · updated
            {{ task.updated_at | date: 'MMM d, HH:mm' }}
            @if (task.closed_at) {
              · closed {{ task.closed_at | date: 'MMM d, HH:mm' }}
            }
          </p>
        </section>
      </div>
    </div>
  `,
  styles: [
    `
      .dialog-backdrop {
        position: fixed;
        inset: 0;
        z-index: 50;
        display: flex;
        align-items: flex-start;
        justify-content: center;
        padding: 6vh 16px;
        overflow-y: auto;
        background: rgba(0, 0, 0, 0.4);
      }
      .dialog {
        width: min(640px, 100%);
        display: flex;
        flex-direction: column;
        gap: 14px;
        outline: none;
      }
      .dialog.is-operator {
        box-shadow:
          inset 3px 0 0 var(--mf-accent),
          var(--mf-shadow);
      }
      .dialog-head {
        display: flex;
        align-items: center;
        justify-content: space-between;
        gap: 10px;
      }
      .dialog-meta {
        display: flex;
        align-items: center;
        gap: 8px;
        flex-wrap: wrap;
      }
      .task-key {
        font-family: var(--mf-font-mono);
        font-size: var(--mf-fs-sm);
        color: var(--mf-text-muted);
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
      .sr-only {
        position: absolute;
        width: 1px;
        height: 1px;
        overflow: hidden;
        clip: rect(0 0 0 0);
        white-space: nowrap;
      }
      .fields {
        display: flex;
        flex-direction: column;
        gap: 12px;
      }
      .grid {
        display: grid;
        grid-template-columns: repeat(4, minmax(0, 1fr));
        gap: 10px;
      }
      @media (max-width: 640px) {
        .grid {
          grid-template-columns: repeat(2, minmax(0, 1fr));
        }
      }
      .actions {
        display: flex;
        align-items: center;
        gap: 8px;
      }
      .spacer {
        flex: 1;
      }
      .notes {
        display: flex;
        flex-direction: column;
        gap: 10px;
        border-top: 1px solid var(--mf-border);
      }
      .notes .mf-section {
        margin-bottom: 0;
      }
      .notes-body {
        margin: 0;
        max-height: 240px;
        overflow: auto;
        white-space: pre-wrap;
        overflow-wrap: anywhere;
        font-family: var(--mf-font-mono);
        font-size: var(--mf-fs-xs);
        line-height: var(--mf-lh-base);
        background: var(--mf-code-bg);
        border: 1px solid var(--mf-border);
        border-radius: var(--mf-radius-sm);
        padding: 10px 12px;
      }
      .none {
        margin: 0;
        font-size: var(--mf-fs-sm);
      }
      .stamps {
        margin: 0;
        font-size: var(--mf-fs-xs);
        color: var(--mf-text-faint);
      }
    `,
  ],
})
export class WorkTaskDetailComponent implements OnChanges, AfterViewInit, OnDestroy {
  private work = inject(WorkService);
  private toast = inject(ToastService);

  @Input({ required: true }) task!: WorkTask;
  @Input({ required: true }) businessId = '';
  @Input({ required: true }) projectId = '';
  @Output() saved = new EventEmitter<WorkTask>();
  @Output() closed = new EventEmitter<void>();

  readonly UserRound = UserRound;
  readonly X = X;
  readonly statuses = STATUSES;
  readonly lanes = LANES;
  readonly types = TYPES;
  readonly priorities = PRIORITIES;
  readonly statusLabel = workStatusLabel;
  readonly statusTone = workStatusTone;
  readonly laneLabel = laneLabel;
  readonly priorityTitle = priorityTitle;
  readonly isOpen = isOpen;

  private card = viewChild<ElementRef<HTMLElement>>('card');
  private titleInput = viewChild<ElementRef<HTMLInputElement>>('titleInput');
  private noteInput = viewChild<ElementRef<HTMLTextAreaElement>>('noteInput');
  private readonly opener = document.activeElement as HTMLElement | null;

  draft = blankDraft();
  noteText = '';
  // 'save' | 'note' | 'done' while a PATCH is in flight.
  busy = signal<'' | 'save' | 'note' | 'done'>('');

  ngOnChanges(changes: SimpleChanges): void {
    if (!changes['task']) return;
    const prev = changes['task'].previousValue as WorkTask | undefined;
    if (!prev || prev.key !== this.task.key) {
      this.draft = toDraft(this.task);
      this.noteText = '';
      return;
    }
    // A background refresh of the same task: take the server's value for every field the user
    // hasn't touched, keep the ones they're editing.
    const before = toDraft(prev);
    const after = toDraft(this.task);
    const draft = this.draft as unknown as Record<string, unknown>;
    for (const k of Object.keys(after) as (keyof Draft)[]) {
      if (JSON.stringify(draft[k]) === JSON.stringify(before[k])) draft[k] = after[k];
    }
  }

  ngAfterViewInit(): void {
    setTimeout(() => this.titleInput()?.nativeElement.focus());
  }

  dirty(): boolean {
    return Object.keys(this.changes()).length > 0;
  }

  changes(): WorkTaskPatch {
    const d = this.draft;
    const t = this.task;
    const patch: WorkTaskPatch = {};
    if (d.title.trim() !== t.title) patch.title = d.title.trim();
    if (d.description !== t.description) patch.description = d.description;
    if (d.status !== t.status) patch.status = d.status;
    if (d.lane !== t.lane) patch.lane = d.lane;
    if (Number(d.priority) !== t.priority) patch.priority = Number(d.priority);
    if (d.type !== t.type) patch.type = d.type;
    if (d.assignee.trim() !== t.assignee) patch.assignee = d.assignee.trim();
    if (!sameList(d.labels, t.labels)) patch.labels = d.labels;
    if (!sameList(d.blocked_by, t.blocked_by)) patch.blocked_by = d.blocked_by;
    return patch;
  }

  save(): void {
    const patch = this.changes();
    if (!Object.keys(patch).length || !this.draft.title.trim() || this.busy()) return;
    this.send('save', patch, 'Task saved', (t) => (this.draft = toDraft(t)));
  }

  addNote(): void {
    const note = this.noteText.trim();
    if (!note || this.busy()) return;
    this.send('note', { note }, 'Note added', () => {
      this.noteText = '';
      this.noteInput()?.nativeElement.focus();
    });
  }

  markDone(): void {
    if (this.busy()) return;
    this.send('done', { status: 'done' }, 'Marked done', (t) => (this.draft.status = t.status));
  }

  close(): void {
    this.closed.emit();
  }

  // Escape is handled at the document so it still works after focus falls out of the dialog
  // (e.g. the button that was just clicked became disabled).
  onEscape(): void {
    this.close();
  }

  onKeydown(event: KeyboardEvent): void {
    trapTab(event, this.card()?.nativeElement);
  }

  // Hand focus back to whatever opened the dialog.
  ngOnDestroy(): void {
    this.opener?.focus();
  }

  private send(
    kind: 'save' | 'note' | 'done',
    patch: WorkTaskPatch,
    ok: string,
    after: (t: WorkTask) => void,
  ): void {
    this.busy.set(kind);
    this.work.updateTask(this.businessId, this.projectId, this.task.key, patch).subscribe({
      next: (t) => {
        this.busy.set('');
        this.task = t;
        after(t);
        this.saved.emit(t);
        this.toast.success(ok);
      },
      error: (e: HttpErrorResponse) => {
        this.busy.set('');
        this.toast.error(describeWorkMutation(e, 'Could not update the task'));
      },
    });
  }
}

interface Draft {
  title: string;
  description: string;
  status: WorkStatus;
  lane: WorkLane;
  priority: number;
  type: WorkType;
  assignee: string;
  labels: string[];
  blocked_by: string[];
}

function blankDraft(): Draft {
  return {
    title: '',
    description: '',
    status: 'todo',
    lane: 'agent',
    priority: 2,
    type: 'task',
    assignee: '',
    labels: [],
    blocked_by: [],
  };
}

function toDraft(t: WorkTask): Draft {
  return {
    title: t.title,
    description: t.description,
    status: t.status,
    lane: t.lane,
    priority: t.priority,
    type: t.type,
    assignee: t.assignee,
    labels: [...t.labels],
    blocked_by: [...t.blocked_by],
  };
}
