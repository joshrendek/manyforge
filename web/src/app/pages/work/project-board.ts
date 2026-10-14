import { DOCUMENT } from '@angular/common';
import { HttpErrorResponse } from '@angular/common/http';
import { Component, DestroyRef, OnInit, computed, inject, signal } from '@angular/core';
import { takeUntilDestroyed } from '@angular/core/rxjs-interop';
import { FormsModule } from '@angular/forms';
import { ActivatedRoute, Router, RouterLink } from '@angular/router';
import { LucideAngularModule, Plus, UserRound } from 'lucide-angular';
import { Subject, catchError, filter, fromEvent, merge, of, switchMap, timer } from 'rxjs';
import {
  WorkLane,
  WorkProject,
  WorkService,
  WorkTask,
  WorkTaskPatch,
} from '../../core/work.service';
import { EmptyState } from '../../ui/empty-state/empty-state';
import { PageHeader } from '../../ui/page-header/page-header';
import { Spinner } from '../../ui/spinner/spinner';
import { StatusPill } from '../../ui/status-pill/status-pill';
import { ToastService } from '../../ui/toast/toast.service';
import { WorkConnectPanelComponent } from './connect-panel';
import { WorkOperatorInboxComponent } from './operator-inbox';
import { WorkNewTaskComponent } from './new-task';
import { WorkTaskCardComponent } from './task-card';
import { WorkTaskDetailComponent } from './task-detail';
import {
  ACTIVE_COLUMNS,
  PRIORITIES,
  byClosedDesc,
  byPriority,
  describeWorkMutation,
  isOpen,
  priorityLabel,
  workStatusLabel,
} from './work-labels';

export type WorkTab = 'board' | 'backlog' | 'operator' | 'connect';
type LaneFilter = 'all' | WorkLane;

const TABS: WorkTab[] = ['board', 'backlog', 'operator', 'connect'];
const REFRESH_MS = 20_000;
const DONE_COLUMN_CAP = 25;

// Work project page (/work/:businessId/:pid). One task list feeds four tabs: the active Board
// (todo → in progress → blocked → done, filterable by lane), the Backlog, the Operator inbox
// (open operator-lane tasks — what the humans have to do), and Connect (mf CLI + tokens).
// Agents write through `mf sync`, so tasks re-poll every 20s while the tab is visible and right
// after any mutation. Status moves use a select / buttons, never drag-and-drop.
@Component({
  selector: 'app-work-project-board',
  imports: [
    FormsModule,
    RouterLink,
    LucideAngularModule,
    PageHeader,
    EmptyState,
    Spinner,
    StatusPill,
    WorkTaskDetailComponent,
    WorkNewTaskComponent,
    WorkConnectPanelComponent,
    WorkTaskCardComponent,
    WorkOperatorInboxComponent,
  ],
  template: `
    <div class="mf-card" data-testid="work-project-page">
      <mf-page-header
        [title]="project()?.name ?? 'Work project'"
        [subtitle]="project() ? project()!.slug + ' · keys ' + project()!.key_prefix + '-…' : ''"
      >
        <ng-container actions>
          @if (project()) {
            <span title="Refreshes every 20 seconds while this tab is open">
              <mf-status-pill tone="success" label="Live" [live]="refreshing()" outline />
            </span>
          }
          <a class="mf-btn mf-btn-ghost mf-btn-sm" routerLink="/work" data-testid="work-back"
            >All projects</a
          >
        </ng-container>
      </mf-page-header>

      @if (loading()) {
        <div class="mf-loading-row" data-testid="work-project-loading">
          <mf-spinner /><span>Loading project…</span>
        </div>
      } @else if (error()) {
        <div class="mf-empty-inline">
          <p class="mf-err" data-testid="work-project-error">{{ error() }}</p>
          <button class="mf-btn mf-btn-ghost mf-btn-sm" (click)="load()">Try again</button>
        </div>
      } @else if (project(); as p) {
        <div class="toolbar">
          <nav class="tabs" role="tablist" aria-label="Work project views">
            @for (t of tabs; track t) {
              <button
                type="button"
                role="tab"
                class="tab"
                [id]="'work-tab-' + t"
                [attr.aria-controls]="'work-panel-' + t"
                [attr.aria-selected]="tab() === t"
                [class.active]="tab() === t"
                [attr.data-testid]="'work-tab-' + t"
                (click)="setTab(t)"
              >
                {{ tabLabel(t) }}
                @if (t === 'operator' && operatorOpen().length) {
                  <span class="tab-count" data-testid="work-operator-count">{{
                    operatorOpen().length
                  }}</span>
                } @else if (t === 'backlog' && backlog().length) {
                  <span class="tab-count muted">{{ backlog().length }}</span>
                }
              </button>
            }
          </nav>
          @if (tab() !== 'connect') {
            <button
              type="button"
              class="mf-btn mf-btn-primary mf-btn-sm"
              data-testid="work-new-task"
              (click)="newTaskOpen.set(true)"
            >
              <lucide-icon [img]="Plus" [size]="14" aria-hidden="true" />
              {{ tab() === 'operator' ? 'New operator task' : 'New task' }}
            </button>
          }
        </div>

        <!-- BOARD -->
        @if (tab() === 'board') {
          <div role="tabpanel" id="work-panel-board" aria-labelledby="work-tab-board">
            <div class="lane-filter" role="group" aria-label="Filter by lane">
              @for (l of laneFilters; track l.value) {
                <button
                  type="button"
                  class="chip"
                  [class.on]="lane() === l.value"
                  [attr.aria-pressed]="lane() === l.value"
                  [attr.data-testid]="'work-lane-' + l.value"
                  (click)="lane.set(l.value)"
                >
                  {{ l.label }}
                </button>
              }
            </div>
            <div class="columns" data-testid="work-board">
              @for (col of columns(); track col.status) {
                <section
                  class="column"
                  [attr.data-testid]="'work-col-' + col.status"
                  [attr.aria-label]="col.label"
                >
                  <header class="column-head">
                    <span>{{ col.label }}</span>
                    <span class="column-count">{{ col.tasks.length }}</span>
                  </header>
                  @for (t of col.tasks; track t.key) {
                    <app-work-task-card
                      [task]="t"
                      [busy]="isBusy(t.key)"
                      (opened)="open(t)"
                      (moved)="update(t, { status: $event })"
                    />
                  } @empty {
                    <p class="column-empty">
                      {{ col.status === 'done' ? 'Nothing closed recently' : 'Nothing here' }}
                    </p>
                  }
                </section>
              }
            </div>
          </div>
        }

        <!-- BACKLOG -->
        @if (tab() === 'backlog') {
          <div role="tabpanel" id="work-panel-backlog" aria-labelledby="work-tab-backlog">
            <div class="mf-table" data-testid="work-backlog">
              <div class="mf-tr mf-th">
                <span style="flex:0 0 96px">Priority</span>
                <span style="flex:0 0 110px">Key</span>
                <span style="flex:1">Title</span>
                <span style="flex:0 0 90px">Lane</span>
                <span style="flex:0 0 120px"></span>
              </div>
              @for (t of backlog(); track t.key) {
                <div
                  class="mf-tr"
                  data-testid="work-backlog-row"
                  [attr.data-key]="t.key"
                  [class.is-operator]="t.lane === 'operator'"
                >
                  <span style="flex:0 0 96px">
                    <select
                      class="mf-select prio-select"
                      data-testid="work-backlog-priority"
                      [attr.aria-label]="'Priority of ' + t.key"
                      [ngModel]="t.priority"
                      (ngModelChange)="update(t, { priority: +$event })"
                      [disabled]="isBusy(t.key)"
                    >
                      @for (p of priorities; track p) {
                        <option [ngValue]="p">{{ priorityLabel(p) }}</option>
                      }
                    </select>
                  </span>
                  <code style="flex:0 0 110px" class="key">{{ t.key }}</code>
                  <span style="flex:1;min-width:0">
                    <button
                      type="button"
                      class="title-link"
                      data-testid="work-task-open"
                      (click)="open(t)"
                    >
                      {{ t.title }}
                    </button>
                    <span class="type">{{ t.type }}</span>
                  </span>
                  <span style="flex:0 0 90px">
                    @if (t.lane === 'operator') {
                      <span class="lane-tag"
                        ><lucide-icon
                          [img]="UserRound"
                          [size]="12"
                          aria-hidden="true"
                        />Operator</span
                      >
                    } @else {
                      <span class="lane-agent">Agent</span>
                    }
                  </span>
                  <span style="flex:0 0 120px;display:flex;justify-content:flex-end">
                    <button
                      type="button"
                      class="mf-btn mf-btn-ghost mf-btn-sm"
                      data-testid="work-backlog-promote"
                      [disabled]="isBusy(t.key)"
                      (click)="update(t, { status: 'todo' })"
                    >
                      Move to To do
                    </button>
                  </span>
                </div>
              } @empty {
                <mf-empty-state title="Backlog is empty" data-testid="work-backlog-empty">
                  Captured-but-unscheduled work lands here. Agents file into it with
                  <code>mf create --backlog</code>.
                </mf-empty-state>
              }
            </div>
          </div>
        }

        <!-- OPERATOR INBOX -->
        @if (tab() === 'operator') {
          <div role="tabpanel" id="work-panel-operator" aria-labelledby="work-tab-operator">
            <app-work-operator-inbox
              [tasks]="operatorOpen()"
              [done]="operatorDone()"
              [busy]="busy()"
              (opened)="open($event)"
              (changed)="update($event.task, $event.patch, $event.ok)"
            />
          </div>
        }

        <!-- CONNECT -->
        @if (tab() === 'connect') {
          <div role="tabpanel" id="work-panel-connect" aria-labelledby="work-tab-connect">
            <app-work-connect-panel [businessId]="businessId" [project]="p" />
          </div>
        }
      }
    </div>

    @if (selected(); as s) {
      <app-work-task-detail
        [task]="s"
        [businessId]="businessId"
        [projectId]="projectId"
        (saved)="onSaved($event)"
        (closed)="closeDetail()"
      />
    }
    @if (newTaskOpen() && project()) {
      <app-work-new-task
        [businessId]="businessId"
        [projectId]="projectId"
        [defaultLane]="tab() === 'operator' ? 'operator' : 'agent'"
        [defaultStatus]="tab() === 'backlog' ? 'backlog' : 'todo'"
        (created)="onCreated($event)"
        (closed)="newTaskOpen.set(false)"
      />
    }
  `,
  styles: [
    `
      .mf-loading-row {
        display: flex;
        align-items: center;
        gap: 10px;
        color: var(--mf-text-muted);
        font-size: var(--mf-fs-sm);
        padding: 18px 0;
      }
      .toolbar {
        display: flex;
        align-items: center;
        justify-content: space-between;
        gap: 12px;
        border-bottom: 1px solid var(--mf-border);
        margin-bottom: 16px;
        flex-wrap: wrap;
      }
      .tabs {
        display: flex;
        overflow-x: auto;
      }
      .tab {
        display: inline-flex;
        align-items: center;
        gap: 7px;
        padding: 10px 14px;
        border: 0;
        border-bottom: 2px solid transparent;
        margin-bottom: -1px;
        color: var(--mf-text-muted);
        background: transparent;
        font: inherit;
        font-size: var(--mf-fs-base);
        cursor: pointer;
        white-space: nowrap;
      }
      @media (max-width: 640px) {
        .tab {
          padding: 10px 9px;
        }
      }
      .tab:hover {
        color: var(--mf-text);
      }
      .tab.active {
        border-bottom-color: var(--mf-accent);
        color: var(--mf-text);
        font-weight: var(--mf-fw-bold);
      }
      .tab:focus-visible {
        outline: none;
        box-shadow: var(--mf-ring);
      }
      .tab-count {
        font: var(--mf-text-eyebrow);
        color: var(--mf-text-on-accent);
        background: var(--mf-accent);
        padding: 0 6px;
        height: 16px;
        display: inline-flex;
        align-items: center;
        border-radius: var(--mf-radius-xs);
      }
      .tab-count.muted {
        color: var(--mf-text-muted);
        background: var(--mf-surface-2);
      }
      .lane-filter {
        display: flex;
        gap: 6px;
        margin-bottom: 14px;
      }
      .chip {
        font: inherit;
        font-size: var(--mf-fs-sm);
        padding: 4px 12px;
        border-radius: var(--mf-radius-pill);
        border: 1px solid var(--mf-border-strong);
        background: transparent;
        color: var(--mf-text-muted);
        cursor: pointer;
      }
      .chip:hover {
        color: var(--mf-text);
      }
      .chip.on {
        background: var(--mf-accent-soft);
        border-color: var(--mf-accent);
        color: var(--mf-accent-text);
        font-weight: var(--mf-fw-semibold);
      }
      .chip:focus-visible {
        outline: none;
        box-shadow: var(--mf-ring);
      }
      .columns {
        display: grid;
        grid-template-columns: repeat(4, minmax(220px, 1fr));
        gap: 12px;
        overflow-x: auto;
        padding-bottom: 4px;
      }
      .column {
        display: flex;
        flex-direction: column;
        gap: 8px;
        background: var(--mf-surface-2);
        border: 1px solid var(--mf-border);
        border-radius: var(--mf-radius);
        padding: 10px;
        min-height: 160px;
      }
      .column-head {
        display: flex;
        align-items: center;
        justify-content: space-between;
        font: var(--mf-text-eyebrow);
        letter-spacing: var(--mf-tracking-label);
        text-transform: uppercase;
        color: var(--mf-text-muted);
        padding: 2px 2px 4px;
      }
      .column-count {
        color: var(--mf-text-faint);
      }
      .column-empty {
        margin: 6px 2px;
        font-size: var(--mf-fs-sm);
        color: var(--mf-text-faint);
      }
      .mf-tr.is-operator {
        box-shadow: inset 3px 0 0 var(--mf-accent);
      }
      .key {
        font-family: var(--mf-font-mono);
        font-size: var(--mf-fs-xs);
        color: var(--mf-text-muted);
      }
      .title-link {
        all: unset;
        cursor: pointer;
        color: var(--mf-text);
        font-weight: var(--mf-fw-semibold);
        overflow-wrap: anywhere;
      }
      .title-link:hover {
        color: var(--mf-accent-text);
      }
      .title-link:focus-visible {
        border-radius: var(--mf-radius-xs);
        box-shadow: var(--mf-ring);
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
      .lane-agent,
      .type {
        font-size: var(--mf-fs-xs);
        color: var(--mf-text-faint);
      }
      .type {
        margin-left: 8px;
      }
      .blockers {
        margin: 0;
        font-size: var(--mf-fs-xs);
        color: var(--mf-danger-text);
      }
      .prio-select {
        height: var(--mf-control-h-sm);
        font-size: var(--mf-fs-sm);
        width: 72px;
      }
      .when {
        font-size: var(--mf-fs-sm);
        color: var(--mf-text-faint);
      }
      .mf-empty-body code {
        font-family: var(--mf-font-mono);
      }
    `,
  ],
})
export class WorkProjectBoardComponent implements OnInit {
  private route = inject(ActivatedRoute);
  private router = inject(Router);
  private work = inject(WorkService);
  private toast = inject(ToastService);
  private document = inject(DOCUMENT);
  private destroyRef = inject(DestroyRef);

  readonly Plus = Plus;
  readonly UserRound = UserRound;
  readonly tabs = TABS;
  readonly priorities = PRIORITIES;
  readonly laneFilters: { value: LaneFilter; label: string }[] = [
    { value: 'all', label: 'All' },
    { value: 'agent', label: 'Agent' },
    { value: 'operator', label: 'Operator' },
  ];
  readonly priorityLabel = priorityLabel;

  businessId = '';
  projectId = '';

  project = signal<WorkProject | null>(null);
  tasks = signal<WorkTask[]>([]);
  loading = signal(true);
  refreshing = signal(false);
  error = signal('');
  tab = signal<WorkTab>('board');
  lane = signal<LaneFilter>('all');
  busy = signal<ReadonlySet<string>>(new Set());
  selectedKey = signal('');
  newTaskOpen = signal(false);

  readonly columns = computed(() => {
    const lane = this.lane();
    const visible = this.tasks().filter((t) => lane === 'all' || t.lane === lane);
    return ACTIVE_COLUMNS.map((c) => {
      const tasks = visible.filter((t) => t.status === c.status);
      return {
        ...c,
        tasks:
          c.status === 'done'
            ? tasks.sort(byClosedDesc).slice(0, DONE_COLUMN_CAP)
            : tasks.sort(byPriority),
      };
    });
  });
  readonly backlog = computed(() =>
    this.tasks()
      .filter((t) => t.status === 'backlog')
      .sort(byPriority),
  );
  readonly operatorOpen = computed(() =>
    this.tasks()
      .filter((t) => t.lane === 'operator' && isOpen(t))
      .sort(byPriority),
  );
  readonly operatorDone = computed(() =>
    this.tasks()
      .filter((t) => t.lane === 'operator' && t.status === 'done')
      .sort(byClosedDesc)
      .slice(0, 5),
  );
  // Looked up from the live list so a background refresh flows into an open dialog.
  readonly selected = computed(
    () => this.tasks().find((t) => t.key === this.selectedKey()) ?? null,
  );

  private readonly refresh$ = new Subject<void>();

  ngOnInit(): void {
    this.businessId = this.route.snapshot.paramMap.get('businessId') ?? '';
    this.projectId = this.route.snapshot.paramMap.get('pid') ?? '';
    const initial = this.route.snapshot.queryParamMap.get('tab') as WorkTab | null;
    if (initial && TABS.includes(initial)) this.tab.set(initial);

    // Single in-flight task fetch: a newer refresh cancels an older one.
    this.refresh$
      .pipe(
        switchMap(() => {
          this.refreshing.set(true);
          return this.work.listTasks(this.businessId, this.projectId).pipe(
            catchError(() => {
              this.refreshing.set(false);
              return of(null);
            }),
          );
        }),
        takeUntilDestroyed(this.destroyRef),
      )
      .subscribe((r) => {
        this.refreshing.set(false);
        if (r) this.tasks.set(r.items ?? []);
      });

    const visible = () => this.document.visibilityState === 'visible';
    merge(
      timer(REFRESH_MS, REFRESH_MS).pipe(filter(visible)),
      fromEvent(this.document, 'visibilitychange').pipe(filter(visible)),
    )
      .pipe(
        filter(() => !!this.project()),
        takeUntilDestroyed(this.destroyRef),
      )
      .subscribe(() => this.refresh$.next());

    this.load();
  }

  load(): void {
    if (!this.businessId || !this.projectId) {
      this.loading.set(false);
      this.error.set("We couldn't load this project.");
      return;
    }
    this.loading.set(true);
    this.error.set('');
    this.work.getProject(this.businessId, this.projectId).subscribe({
      next: (p) => {
        this.project.set(p);
        this.loading.set(false);
        this.refresh$.next();
      },
      error: (e: HttpErrorResponse) => {
        this.loading.set(false);
        this.error.set(
          e.status === 403 || e.status === 404
            ? "This project doesn't exist or you don't have access to it."
            : "We couldn't load this project.",
        );
      },
    });
  }

  tabLabel(t: WorkTab): string {
    return { board: 'Board', backlog: 'Backlog', operator: 'Operator', connect: 'Connect' }[t];
  }

  setTab(t: WorkTab): void {
    if (this.tab() === t) return;
    this.tab.set(t);
    void this.router.navigate([], {
      relativeTo: this.route,
      queryParams: { tab: t === 'board' ? null : t },
      queryParamsHandling: 'merge',
      replaceUrl: true,
    });
  }

  open(t: WorkTask): void {
    this.selectedKey.set(t.key);
  }

  closeDetail(): void {
    this.selectedKey.set('');
  }

  isBusy(key: string): boolean {
    return this.busy().has(key);
  }

  update(t: WorkTask, patch: WorkTaskPatch, ok?: string): void {
    if (this.isBusy(t.key)) return;
    if (patch.status && patch.status === t.status) return;
    if (patch.priority !== undefined && patch.priority === t.priority) return;
    this.setBusy(t.key, true);
    this.work.updateTask(this.businessId, this.projectId, t.key, patch).subscribe({
      next: (updated) => {
        this.setBusy(t.key, false);
        this.patchTask(updated);
        this.toast.success(
          ok ?? (patch.status ? `${t.key} → ${workStatusLabel(patch.status)}` : `${t.key} updated`),
        );
        this.afterMutation();
      },
      error: (e: HttpErrorResponse) => {
        this.setBusy(t.key, false);
        this.toast.error(describeWorkMutation(e, 'Could not update the task'));
        // Re-read so selects snap back to the server's truth.
        this.refresh$.next();
      },
    });
  }

  onSaved(t: WorkTask): void {
    this.patchTask(t);
    this.afterMutation();
  }

  onCreated(t: WorkTask): void {
    this.newTaskOpen.set(false);
    this.tasks.set([...this.tasks(), t]);
    this.afterMutation();
  }

  private setBusy(key: string, on: boolean): void {
    const next = new Set(this.busy());
    if (on) next.add(key);
    else next.delete(key);
    this.busy.set(next);
  }

  private patchTask(updated: WorkTask): void {
    const list = this.tasks();
    this.tasks.set(
      list.some((x) => x.key === updated.key)
        ? list.map((x) => (x.key === updated.key ? updated : x))
        : [...list, updated],
    );
  }

  private afterMutation(): void {
    this.work.refreshSummary(this.businessId);
    this.refresh$.next();
  }
}
