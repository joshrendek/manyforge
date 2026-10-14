import { HttpErrorResponse } from '@angular/common/http';
import {
  AfterViewInit,
  Component,
  ElementRef,
  EventEmitter,
  Input,
  OnDestroy,
  OnInit,
  Output,
  inject,
  signal,
  viewChild,
} from '@angular/core';
import { FormsModule } from '@angular/forms';
import { WorkLane, WorkService, WorkTask, WorkType } from '../../core/work.service';
import { ToastService } from '../../ui/toast/toast.service';
import {
  LANES,
  PRIORITIES,
  TYPES,
  describeWorkMutation,
  laneLabel,
  priorityTitle,
  trapTab,
} from './work-labels';

// "New task" dialog. The server generates the key. Lane/status defaults come from the tab it was
// opened on (the Operator tab files operator asks; Backlog files into the backlog).
@Component({
  selector: 'app-work-new-task',
  host: { '(document:keydown.escape)': 'onEscape()' },
  imports: [FormsModule],
  template: `
    <div
      class="dialog-backdrop"
      data-testid="work-new-task-dialog"
      role="dialog"
      aria-modal="true"
      aria-labelledby="work-new-task-title"
      (click)="closed.emit()"
      (keydown)="onKeydown($event)"
    >
      <form class="mf-card dialog" #card (click)="$event.stopPropagation()" (ngSubmit)="submit()">
        <h2 id="work-new-task-title">
          {{ lane === 'operator' ? 'New operator task' : 'New task' }}
        </h2>
        @if (lane === 'operator') {
          <p class="hint">
            Operator tasks are for things only a person can do. Put the exact steps in the
            description.
          </p>
        }
        <div class="mf-field">
          <label for="wn-title">Title</label>
          <input
            #titleInput
            id="wn-title"
            class="mf-input"
            name="title"
            data-testid="work-new-title"
            [placeholder]="
              lane === 'operator' ? 'e.g. Rotate the Stripe live key' : 'What needs doing?'
            "
            [(ngModel)]="title"
          />
        </div>
        <div class="mf-field">
          <label for="wn-desc">Description</label>
          <textarea
            id="wn-desc"
            class="mf-textarea"
            name="description"
            rows="4"
            placeholder="Optional"
            data-testid="work-new-description"
            [(ngModel)]="description"
          ></textarea>
        </div>
        <div class="grid">
          <div class="mf-field">
            <label for="wn-lane">Lane</label>
            <select
              id="wn-lane"
              class="mf-select"
              name="lane"
              data-testid="work-new-lane"
              [(ngModel)]="lane"
            >
              @for (l of lanes; track l) {
                <option [value]="l">{{ laneLabel(l) }}</option>
              }
            </select>
          </div>
          <div class="mf-field">
            <label for="wn-status">Goes to</label>
            <select
              id="wn-status"
              class="mf-select"
              name="status"
              data-testid="work-new-status"
              [(ngModel)]="status"
            >
              <option value="todo">To do</option>
              <option value="backlog">Backlog</option>
            </select>
          </div>
          <div class="mf-field">
            <label for="wn-priority">Priority</label>
            <select
              id="wn-priority"
              class="mf-select"
              name="priority"
              data-testid="work-new-priority"
              [(ngModel)]="priority"
            >
              @for (p of priorities; track p) {
                <option [ngValue]="p">{{ priorityTitle(p) }}</option>
              }
            </select>
          </div>
          <div class="mf-field">
            <label for="wn-type">Type</label>
            <select
              id="wn-type"
              class="mf-select"
              name="type"
              data-testid="work-new-type"
              [(ngModel)]="type"
            >
              @for (t of types; track t) {
                <option [value]="t">{{ t }}</option>
              }
            </select>
          </div>
        </div>
        <div class="actions">
          <button type="button" class="mf-btn mf-btn-ghost mf-btn-sm" (click)="closed.emit()">
            Cancel
          </button>
          <button
            type="submit"
            class="mf-btn mf-btn-primary mf-btn-sm"
            data-testid="work-new-submit"
            [disabled]="!title.trim() || creating()"
          >
            {{ creating() ? 'Creating…' : 'Create task' }}
          </button>
        </div>
      </form>
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
        padding: 10vh 16px;
        overflow-y: auto;
        background: rgba(0, 0, 0, 0.4);
      }
      .dialog {
        width: min(560px, 100%);
        display: flex;
        flex-direction: column;
        gap: 12px;
      }
      .dialog h2 {
        margin: 0;
        font: var(--mf-text-h2);
      }
      .hint {
        margin: 0;
        color: var(--mf-text-muted);
        font-size: var(--mf-fs-sm);
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
        justify-content: flex-end;
        gap: 8px;
      }
    `,
  ],
})
export class WorkNewTaskComponent implements OnInit, AfterViewInit, OnDestroy {
  private work = inject(WorkService);
  private toast = inject(ToastService);

  @Input({ required: true }) businessId = '';
  @Input({ required: true }) projectId = '';
  @Input() defaultLane: WorkLane = 'agent';
  @Input() defaultStatus: 'todo' | 'backlog' = 'todo';
  @Output() created = new EventEmitter<WorkTask>();
  @Output() closed = new EventEmitter<void>();

  readonly lanes = LANES;
  readonly types = TYPES;
  readonly priorities = PRIORITIES;
  readonly laneLabel = laneLabel;
  readonly priorityTitle = priorityTitle;

  private card = viewChild<ElementRef<HTMLElement>>('card');
  private titleInput = viewChild<ElementRef<HTMLInputElement>>('titleInput');
  private readonly opener = document.activeElement as HTMLElement | null;

  title = '';
  description = '';
  lane: WorkLane = 'agent';
  status: 'todo' | 'backlog' = 'todo';
  priority = 2;
  type: WorkType = 'task';
  creating = signal(false);

  ngOnInit(): void {
    this.lane = this.defaultLane;
    this.status = this.defaultStatus;
  }

  ngAfterViewInit(): void {
    setTimeout(() => this.titleInput()?.nativeElement.focus());
  }

  submit(): void {
    const title = this.title.trim();
    if (!title || this.creating()) return;
    this.creating.set(true);
    const description = this.description.trim();
    this.work
      .createTask(this.businessId, this.projectId, {
        title,
        ...(description ? { description } : {}),
        lane: this.lane,
        status: this.status,
        priority: Number(this.priority),
        type: this.type,
      })
      .subscribe({
        next: (t) => {
          this.creating.set(false);
          this.toast.success(`Created ${t.key}`);
          this.created.emit(t);
        },
        error: (e: HttpErrorResponse) => {
          this.creating.set(false);
          this.toast.error(describeWorkMutation(e, 'Could not create the task'));
        },
      });
  }

  // Escape is handled at the document so it still works after focus falls out of the dialog
  // (e.g. the button that was just clicked became disabled).
  onEscape(): void {
    this.closed.emit();
  }

  onKeydown(event: KeyboardEvent): void {
    trapTab(event, this.card()?.nativeElement);
  }

  // Hand focus back to whatever opened the dialog.
  ngOnDestroy(): void {
    this.opener?.focus();
  }
}
