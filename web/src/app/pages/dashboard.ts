import { Component, OnInit, computed, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { HttpErrorResponse } from '@angular/common/http';
import { Observable } from 'rxjs';
import { Router, RouterLink } from '@angular/router';
import { AuthService } from '../core/auth.service';
import { BusinessService } from '../core/business.service';
import { Business, Row, buildTree, flatten } from '../core/tree';
import { PageHeader } from '../ui/page-header/page-header';
import { StatusPill } from '../ui/status-pill/status-pill';
import { EmptyState } from '../ui/empty-state/empty-state';

type PanelKind = 'add' | 'rename' | 'move';

@Component({
  selector: 'app-dashboard',
  imports: [FormsModule, RouterLink, PageHeader, StatusPill, EmptyState],
  template: `
    <div class="mf-card">
      <mf-page-header title="Your businesses" subtitle="Manage your tenant businesses and their hierarchy.">
        <a class="mf-btn mf-btn-ghost mf-btn-sm" routerLink="/accounting" data-testid="nav-accounting" actions>Accounting</a>
      </mf-page-header>

      @if (loading()) {
        <p class="mf-text-muted">Loading your businesses…</p>
      } @else if (loadFailed()) {
        <div class="mf-err">
          <p>We couldn't load your businesses.</p>
          <button class="mf-btn mf-btn-ghost mf-btn-sm" (click)="loadBusinesses()">Try again</button>
        </div>
      } @else {
        <ul class="mf-tree">
          @for (row of rows(); track row.business.id) {
            <li
              class="mf-tr"
              data-testid="biz-row"
              [class.is-child]="row.depth > 0"
              [style.paddingLeft.px]="row.depth * 22 + 16"
            >
              <div class="mf-tr-main">
                @if (row.hasChildren) {
                  <button
                    class="mf-btn mf-btn-ghost mf-btn-sm mf-caret"
                    [attr.aria-label]="row.collapsed ? 'Expand' : 'Collapse'"
                    (click)="toggle(row.business.id)"
                  >{{ row.collapsed ? '▸' : '▾' }}</button>
                } @else {
                  <span class="mf-caret-spacer"></span>
                }
                <span class="mf-tr-name" [class.mf-text-muted]="row.business.status === 'archived'">{{ row.business.name }}</span>
                @if (row.business.is_tenant_root) {
                  <mf-status-pill tone="accent" label="master" />
                }
                @if (row.business.status === 'archived') {
                  <mf-status-pill tone="neutral" label="archived" />
                }
              </div>

              <div class="mf-tr-actions">
                <button class="mf-btn mf-btn-ghost mf-btn-sm" (click)="openPanel('add', row)">Add sub</button>
                <button class="mf-btn mf-btn-ghost mf-btn-sm" (click)="openPanel('rename', row)">Rename</button>
                @if (!row.business.is_tenant_root) {
                  <button class="mf-btn mf-btn-ghost mf-btn-sm" (click)="openPanel('move', row)">Move</button>
                }
                @if (row.business.status === 'archived') {
                  <button class="mf-btn mf-btn-ghost mf-btn-sm" (click)="restore(row.business)">Restore</button>
                } @else {
                  <button class="mf-btn mf-btn-ghost mf-btn-sm" (click)="archive(row.business)">Archive</button>
                }
                <button class="mf-btn mf-btn-link mf-btn-sm mf-btn-danger" (click)="askDelete(row.business)">Delete</button>
              </div>

              @if (panel()?.id === row.business.id) {
                <div class="mf-card mf-panel">
                  @switch (panel()!.kind) {
                    @case ('add') {
                      <div class="mf-field">
                        <label [attr.for]="'sub-' + row.business.id">New sub-business under {{ row.business.name }}</label>
                        <div class="mf-field-row">
                          <input
                            class="mf-input"
                            [id]="'sub-' + row.business.id"
                            data-testid="sub-name-input"
                            type="text"
                            [(ngModel)]="draftName"
                            placeholder="e.g. Engineering"
                            (keyup.enter)="createSub(row.business)"
                          />
                          <button class="mf-btn mf-btn-primary mf-btn-sm" [disabled]="busy()" (click)="createSub(row.business)">Create sub-business</button>
                          <button class="mf-btn mf-btn-ghost mf-btn-sm" (click)="closePanel()">Cancel</button>
                        </div>
                      </div>
                    }
                    @case ('rename') {
                      <div class="mf-field">
                        <label [attr.for]="'rename-' + row.business.id">Rename {{ row.business.name }}</label>
                        <div class="mf-field-row">
                          <input
                            class="mf-input"
                            [id]="'rename-' + row.business.id"
                            type="text"
                            [(ngModel)]="draftName"
                            (keyup.enter)="rename(row.business)"
                          />
                          <button class="mf-btn mf-btn-primary mf-btn-sm" [disabled]="busy()" (click)="rename(row.business)">Save name</button>
                          <button class="mf-btn mf-btn-ghost mf-btn-sm" (click)="closePanel()">Cancel</button>
                        </div>
                      </div>
                    }
                    @case ('move') {
                      <div class="mf-field">
                        <label [attr.for]="'move-' + row.business.id">Move {{ row.business.name }} under</label>
                        <div class="mf-field-row">
                          <select class="mf-select" [id]="'move-' + row.business.id" [(ngModel)]="draftTarget">
                            <option value="" disabled>Choose a new parent…</option>
                            @for (t of moveTargets(row.business); track t.id) {
                              <option [value]="t.id">{{ t.label }}</option>
                            }
                          </select>
                          <button class="mf-btn mf-btn-primary mf-btn-sm" [disabled]="busy() || !draftTarget" (click)="move(row.business)">Move here</button>
                          <button class="mf-btn mf-btn-ghost mf-btn-sm" (click)="closePanel()">Cancel</button>
                        </div>
                      </div>
                    }
                  }
                </div>
              }

              @if (confirmDelete()?.id === row.business.id) {
                <div class="mf-card mf-panel mf-panel-danger">
                  <span>Delete <b>{{ row.business.name }}</b>? This can't be undone.</span>
                  <div class="mf-field-row">
                    <button class="mf-btn mf-btn-danger mf-btn-sm" [disabled]="busy()" (click)="doDelete(row.business)">Confirm delete</button>
                    <button class="mf-btn mf-btn-ghost mf-btn-sm" (click)="confirmDelete.set(null)">Cancel</button>
                  </div>
                </div>
              }
            </li>
          } @empty {
            <mf-empty-state icon="🏢" title="No businesses yet — create your master business below." />
          }
        </ul>
      }

      @if (error()) { <p class="mf-err">{{ error() }}</p> }
    </div>

    <div class="mf-card" style="margin-top:var(--mf-space-4)">
      <mf-page-header title="Create a master business" subtitle="A master business is the root of its own tenant — fully isolated from your others." />
      <form (ngSubmit)="createMaster()">
        <div class="mf-field">
          <label for="bizname">Business name</label>
          <input class="mf-input" id="bizname" type="text" name="name" [(ngModel)]="masterName" placeholder="Acme, Inc." required />
        </div>
        <button class="mf-btn mf-btn-primary" type="submit" [disabled]="busy()">{{ busy() ? 'Working…' : 'Create master business' }}</button>
      </form>
    </div>
  `,
})
export class DashboardComponent implements OnInit {
  private auth = inject(AuthService);
  private api = inject(BusinessService);
  private router = inject(Router);

  businesses = signal<Business[]>([]);
  collapsed = signal<ReadonlySet<string>>(new Set());
  loading = signal(true);
  loadFailed = signal(false);
  busy = signal(false);
  error = signal('');

  panel = signal<{ id: string; kind: PanelKind } | null>(null);
  confirmDelete = signal<{ id: string } | null>(null);
  draftName = '';
  draftTarget = '';
  masterName = '';

  readonly rows = computed<Row[]>(() => flatten(buildTree(this.businesses()), this.collapsed()));

  ngOnInit(): void {
    this.auth.me().subscribe({ next: () => {}, error: () => this.forceLogin() });
    this.loadBusinesses();
  }

  loadBusinesses(): void {
    this.loading.set(true);
    this.loadFailed.set(false);
    this.api.list().subscribe({
      next: (r) => {
        this.businesses.set(r.items ?? []);
        this.loading.set(false);
      },
      error: () => {
        this.loading.set(false);
        this.loadFailed.set(true);
      },
    });
  }

  toggle(id: string): void {
    const next = new Set(this.collapsed());
    next.has(id) ? next.delete(id) : next.add(id);
    this.collapsed.set(next);
  }

  // moveTargets offers every business in the same tenant root except the node
  // itself and its descendants (which would form a cycle). The backend enforces
  // the same rules; this just keeps the menu honest.
  moveTargets(node: Business): { id: string; label: string }[] {
    const all = this.businesses();
    const banned = this.descendantIds(node.id);
    banned.add(node.id);
    if (node.parent_id) banned.add(node.parent_id); // already there
    return all
      .filter((b) => b.tenant_root_id === node.tenant_root_id && b.status === 'active' && !banned.has(b.id))
      .map((b) => ({ id: b.id, label: b.is_tenant_root ? `${b.name} (master)` : b.name }))
      .sort((a, b) => a.label.localeCompare(b.label));
  }

  private descendantIds(id: string): Set<string> {
    const byParent = new Map<string, Business[]>();
    for (const b of this.businesses()) {
      if (b.parent_id) byParent.set(b.parent_id, [...(byParent.get(b.parent_id) ?? []), b]);
    }
    const out = new Set<string>();
    const walk = (pid: string) => {
      for (const c of byParent.get(pid) ?? []) {
        if (!out.has(c.id)) {
          out.add(c.id);
          walk(c.id);
        }
      }
    };
    walk(id);
    return out;
  }

  openPanel(kind: PanelKind, row: Row): void {
    this.error.set('');
    this.confirmDelete.set(null);
    this.draftName = kind === 'rename' ? row.business.name : '';
    this.draftTarget = '';
    this.panel.set({ id: row.business.id, kind });
  }

  closePanel(): void {
    this.panel.set(null);
  }

  askDelete(b: Business): void {
    this.error.set('');
    this.panel.set(null);
    this.confirmDelete.set({ id: b.id });
  }

  createMaster(): void {
    const name = this.masterName.trim();
    if (!name) return;
    this.run(this.api.create(name), () => (this.masterName = ''), 'create');
  }

  createSub(parent: Business): void {
    const name = this.draftName.trim();
    if (!name) return;
    this.run(this.api.create(name, parent.id), () => this.closePanel(), 'create');
  }

  rename(b: Business): void {
    const name = this.draftName.trim();
    if (!name) return;
    this.run(this.api.rename(b.id, name), () => this.closePanel(), 'rename');
  }

  move(b: Business): void {
    if (!this.draftTarget) return;
    this.run(this.api.move(b.id, this.draftTarget), () => this.closePanel(), 'move');
  }

  archive(b: Business): void {
    this.run(this.api.archive(b.id), () => {}, 'archive');
  }

  restore(b: Business): void {
    this.run(this.api.restore(b.id), () => {}, 'restore');
  }

  doDelete(b: Business): void {
    this.run(this.api.remove(b.id), () => this.confirmDelete.set(null), 'delete');
  }

  // run centralises the busy flag, error mapping, and reload-on-success so each
  // action handler stays a one-liner.
  private run(obs: Observable<unknown>, onOk: () => void, action: string): void {
    this.busy.set(true);
    this.error.set('');
    obs.subscribe({
      next: () => {
        this.busy.set(false);
        onOk();
        this.loadBusinesses();
      },
      error: (e: HttpErrorResponse) => {
        this.busy.set(false);
        this.error.set(this.describeError(e, action));
      },
    });
  }

  private describeError(e: HttpErrorResponse, action: string): string {
    if (e.status === 409 && action === 'delete') {
      return "This business can't be deleted while it has active sub-businesses. Archive or move them first.";
    }
    if (e.status === 409 && action === 'move') {
      return 'That move would create a cycle or cross tenants. Pick a different parent.';
    }
    if (e.status === 409) return 'That change conflicts with the current state. Refresh and try again.';
    if (e.status === 400 || e.status === 422) return 'That input was rejected. Check the name and try again.';
    if (e.status === 403 || e.status === 404) return "You don't have access to do that.";
    return `Could not ${action} the business. Please try again.`;
  }

  private forceLogin(): void {
    void this.router.navigateByUrl('/login');
  }
}
