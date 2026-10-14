import { takeUntilDestroyed } from '@angular/core/rxjs-interop';
import { Subject, takeUntil } from 'rxjs';
import { HttpErrorResponse } from '@angular/common/http';
import { Component, OnInit, inject, signal, DestroyRef, effect, untracked } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { RouterLink } from '@angular/router';
import { BusinessService } from '../../core/business.service';
import { CurrentBusinessService } from '../../core/current-business.service';
import { Business } from '../../core/tree';
import { WorkProject, WorkService } from '../../core/work.service';
import { EmptyState } from '../../ui/empty-state/empty-state';
import { PageHeader } from '../../ui/page-header/page-header';
import { Spinner } from '../../ui/spinner/spinner';
import { ToastService } from '../../ui/toast/toast.service';

// Work Board project list (/work). Business selection mirrors the feedback boards list (same
// /api/v1/businesses list, persisted via CurrentBusinessService). Each project card shows its
// board counts with the open operator asks called out; an inline form creates a project.
@Component({
  selector: 'app-work-projects-list',
  imports: [FormsModule, RouterLink, PageHeader, EmptyState, Spinner],
  template: `
    <div class="mf-card" data-testid="work-projects-page">
      <mf-page-header
        [eyebrow]="businessName()"
        title="Work"
        subtitle="Agent tasks and the operator asks only a human can do"
      >
        @if (loading()) {
          <span class="mf-loading-row" data-testid="work-projects-loading" actions
            ><mf-spinner
          /></span>
        }
      </mf-page-header>

      @if (businessId()) {
        <form class="mf-filters" data-testid="work-project-new" (ngSubmit)="create()">
          <div class="mf-field" style="flex:2 1 220px">
            <label for="wp-new-name">New project name</label>
            <input
              id="wp-new-name"
              class="mf-input"
              type="text"
              name="newName"
              placeholder="e.g. ManyForge"
              data-testid="work-project-name"
              [(ngModel)]="newName"
            />
          </div>
          <div class="mf-field" style="flex:1 1 150px">
            <label for="wp-new-slug">Slug <span class="opt">optional</span></label>
            <input
              id="wp-new-slug"
              class="mf-input mf-mono"
              type="text"
              name="newSlug"
              placeholder="manyforge"
              data-testid="work-project-slug"
              [(ngModel)]="newSlug"
            />
          </div>
          <div class="mf-field" style="flex:0 1 120px">
            <label for="wp-new-prefix">Key prefix <span class="opt">optional</span></label>
            <input
              id="wp-new-prefix"
              class="mf-input mf-mono"
              type="text"
              name="newPrefix"
              placeholder="mf"
              data-testid="work-project-prefix"
              [(ngModel)]="newPrefix"
            />
          </div>
          <div style="display:flex;align-items:flex-end">
            <button
              type="submit"
              class="mf-btn mf-btn-primary mf-btn-sm"
              data-testid="work-project-create"
              [disabled]="!newName.trim() || creating()"
            >
              {{ creating() ? 'Creating…' : 'New project' }}
            </button>
          </div>
        </form>
      }

      @if (items().length) {
        <div class="projects" data-testid="work-projects-list">
          @for (p of items(); track p.id) {
            <a
              class="project"
              data-testid="work-project-card"
              [attr.data-project-id]="p.id"
              [class.has-asks]="(p.counts?.operator_open ?? 0) > 0"
              [routerLink]="['/work', businessId(), p.id]"
            >
              <div class="project-head">
                <span class="project-name" data-testid="work-project-card-name">{{ p.name }}</span>
                <code class="project-prefix" title="Task key prefix">{{ p.key_prefix }}-</code>
              </div>
              <div class="project-slug">{{ p.slug }}</div>
              <dl class="counts">
                <div>
                  <dt>To do</dt>
                  <dd data-testid="count-todo">{{ p.counts?.todo ?? 0 }}</dd>
                </div>
                <div>
                  <dt>In progress</dt>
                  <dd data-testid="count-in-progress">{{ p.counts?.in_progress ?? 0 }}</dd>
                </div>
                <div>
                  <dt>Blocked</dt>
                  <dd data-testid="count-blocked" [class.is-blocked]="(p.counts?.blocked ?? 0) > 0">
                    {{ p.counts?.blocked ?? 0 }}
                  </dd>
                </div>
                <div class="asks">
                  <dt>Operator</dt>
                  <dd data-testid="count-operator">{{ p.counts?.operator_open ?? 0 }}</dd>
                </div>
              </dl>
            </a>
          }
        </div>
      } @else if (businessId() && !loading() && !error()) {
        <mf-empty-state title="No work projects yet" data-testid="work-projects-empty">
          A work project is a task board your coding agents and your team share. Agents pick up
          <b>agent</b>-lane tasks with the <code>mf</code> CLI and, when they're blocked on a person
          — rotate a key, approve DNS, click install — they file an <b>operator</b> task that shows
          up here. Create a project above, then open its <b>Connect</b> tab to wire a repo.
        </mf-empty-state>
      }

      @if (error()) {
        <div class="mf-empty-inline">
          <p class="mf-err" data-testid="work-projects-error">{{ error() }}</p>
          <button class="mf-btn mf-btn-ghost mf-btn-sm" (click)="reload()">Try again</button>
        </div>
      }
    </div>
  `,
  styles: [
    `
      .mf-loading-row {
        display: flex;
        align-items: center;
        gap: 10px;
      }
      .opt {
        color: var(--mf-text-faint);
        font-weight: var(--mf-fw-regular);
      }
      .projects {
        display: grid;
        grid-template-columns: repeat(auto-fill, minmax(280px, 1fr));
        gap: var(--mf-space-3);
      }
      .project {
        display: flex;
        flex-direction: column;
        gap: 4px;
        padding: 14px 16px;
        border: 1px solid var(--mf-border);
        border-radius: var(--mf-radius-sm);
        background: var(--mf-surface);
        color: var(--mf-text);
        transition: border-color var(--mf-dur-fast) var(--mf-ease);
      }
      .project:hover {
        text-decoration: none;
        border-color: var(--mf-border-strong);
      }
      .project:hover .project-name {
        color: var(--mf-accent-text);
      }
      .project.has-asks {
        box-shadow: inset 3px 0 0 var(--mf-accent);
      }
      .project-head {
        display: flex;
        align-items: baseline;
        justify-content: space-between;
        gap: 10px;
      }
      .project-name {
        font-weight: var(--mf-fw-semibold);
        font-size: var(--mf-fs-md);
      }
      .project-prefix,
      .project-slug {
        font-family: var(--mf-font-mono);
        font-size: var(--mf-fs-xs);
        color: var(--mf-text-muted);
      }
      .counts {
        display: grid;
        grid-template-columns: repeat(4, 1fr);
        gap: 8px;
        margin: 10px 0 0;
      }
      .counts dt {
        font: var(--mf-text-eyebrow);
        letter-spacing: var(--mf-tracking-label);
        text-transform: uppercase;
        color: var(--mf-text-faint);
        white-space: nowrap;
      }
      .counts dd {
        margin: 4px 0 0;
        font-size: var(--mf-fs-lg);
        font-weight: var(--mf-fw-semibold);
        font-variant-numeric: tabular-nums;
      }
      .counts dd.is-blocked {
        color: var(--mf-danger-text);
      }
      .counts .asks dt,
      .counts .asks dd {
        color: var(--mf-accent-text);
      }
      .mf-empty-body code {
        font-family: var(--mf-font-mono);
      }
    `,
  ],
})
export class WorkProjectsListComponent implements OnInit {
  private readonly destroyRef = inject(DestroyRef);
  private readonly businessChanged = new Subject<void>();
  private readonly followBusiness = effect(() => {
    const id = this.current.businessId() ?? '';
    untracked(() => {
      if (id !== this.businessId()) this.selectBusiness(id);
    });
  });
  businessName(): string {
    return this.businesses().find((business) => business.id === this.businessId())?.name ?? '';
  }

  private bizApi = inject(BusinessService);
  private work = inject(WorkService);
  private current = inject(CurrentBusinessService);
  private toast = inject(ToastService);

  businesses = signal<Business[]>([]);
  businessId = signal<string>('');
  items = signal<WorkProject[]>([]);
  loading = signal(false);
  error = signal('');
  newName = '';
  newSlug = '';
  newPrefix = '';
  creating = signal(false);

  ngOnInit(): void {
    this.bizApi
      .list()
      .pipe(takeUntilDestroyed(this.destroyRef))
      .subscribe({
        next: (r) => {
          const items = r.items ?? [];
          this.businesses.set(items);
          const id = this.current.businessId() ?? items[0]?.id;
          if (id) {
            this.selectBusiness(id);
          }
        },
        error: () => this.error.set('Could not load businesses'),
      });
  }

  selectBusiness(id: string): void {
    if (id === this.businessId()) return;
    this.businessChanged.next();
    this.newName = '';
    this.newSlug = '';
    this.newPrefix = '';
    this.items.set([]);
    this.loading.set(false);
    this.error.set('');
    this.creating.set(false);
    this.businessId.set(id);
    if (id) this.current.set(id);
    if (id) this.reload();
  }

  reload(): void {
    if (!this.businessId()) return;
    const biz = this.businessId();
    this.loading.set(true);
    this.work
      .listProjects(biz)
      .pipe(takeUntil(this.businessChanged), takeUntilDestroyed(this.destroyRef))
      .subscribe({
        next: (r) => {
          if (this.businessId() !== biz) return;
          this.items.set(r.items ?? []);
          this.error.set('');
          this.loading.set(false);
        },
        error: () => {
          if (this.businessId() !== biz) return;
          this.items.set([]);
          this.error.set('Could not load work projects');
          this.loading.set(false);
        },
      });
  }

  create(): void {
    const name = this.newName.trim();
    if (!name || this.creating()) return;
    const body: { name: string; slug?: string; key_prefix?: string } = { name };
    const slug = this.newSlug.trim();
    const prefix = this.newPrefix.trim();
    if (slug) body.slug = slug;
    if (prefix) body.key_prefix = prefix;
    this.creating.set(true);
    this.work
      .createProject(this.businessId(), body)
      .pipe(takeUntil(this.businessChanged), takeUntilDestroyed(this.destroyRef))
      .subscribe({
        next: () => {
          this.newName = '';
          this.newSlug = '';
          this.newPrefix = '';
          this.creating.set(false);
          this.toast.success('Project created');
          this.reload();
        },
        error: (e: HttpErrorResponse) => {
          this.creating.set(false);
          this.toast.error(describeCreateError(e));
        },
      });
  }
}

function describeCreateError(e: HttpErrorResponse): string {
  if (e.status === 409) return 'A project with that slug already exists';
  if (e.status === 400) {
    const msg = (e.error as { message?: string } | null)?.message;
    return msg
      ? `That project was rejected: ${msg}`
      : 'That project was rejected. Check your input.';
  }
  if (e.status === 403 || e.status === 404) return "You don't have access to do that.";
  return 'Could not create project';
}
