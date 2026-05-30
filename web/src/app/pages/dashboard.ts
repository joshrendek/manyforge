import { Component, OnInit, computed, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { HttpErrorResponse } from '@angular/common/http';
import { Observable } from 'rxjs';
import { Router } from '@angular/router';
import { AuthService, Profile } from '../core/auth.service';
import { BusinessService } from '../core/business.service';
import { Business, Row, buildTree, flatten } from '../core/tree';

type PanelKind = 'add' | 'rename' | 'move';

@Component({
  selector: 'app-dashboard',
  imports: [FormsModule],
  template: `
    <section class="card">
      <div class="spread">
        <div>
          <h1>Your businesses</h1>
          @if (profile(); as p) {
            <p class="profile">Signed in as <b>{{ p.display_name }}</b> ({{ p.email }})</p>
          }
        </div>
        <button class="ghost compact" (click)="logout()">Sign out</button>
      </div>

      @if (loading()) {
        <p class="empty">Loading your businesses…</p>
      } @else {
        <ul class="tree">
          @for (row of rows(); track row.business.id) {
            <li class="biz" data-testid="biz-row" [style.paddingLeft.px]="row.depth * 22 + 16">
              <div class="biz-main">
                @if (row.hasChildren) {
                  <button
                    class="caret"
                    [attr.aria-label]="row.collapsed ? 'Expand' : 'Collapse'"
                    (click)="toggle(row.business.id)"
                  >{{ row.collapsed ? '▸' : '▾' }}</button>
                } @else {
                  <span class="caret-spacer"></span>
                }
                <span class="name" [class.muted]="row.business.status === 'archived'">{{ row.business.name }}</span>
                @if (row.business.is_tenant_root) { <span class="pill">master</span> }
                @if (row.business.status === 'archived') { <span class="badge">archived</span> }
              </div>

              <div class="biz-actions">
                <button class="linklike" (click)="openPanel('add', row)">Add sub</button>
                <button class="linklike" (click)="openPanel('rename', row)">Rename</button>
                @if (!row.business.is_tenant_root) {
                  <button class="linklike" (click)="openPanel('move', row)">Move</button>
                }
                @if (row.business.status === 'archived') {
                  <button class="linklike" (click)="restore(row.business)">Restore</button>
                } @else {
                  <button class="linklike" (click)="archive(row.business)">Archive</button>
                }
                <button class="linklike danger" (click)="askDelete(row.business)">Delete</button>
              </div>

              @if (panel()?.id === row.business.id) {
                <div class="panel">
                  @switch (panel()!.kind) {
                    @case ('add') {
                      <label [attr.for]="'sub-' + row.business.id">New sub-business under {{ row.business.name }}</label>
                      <div class="row">
                        <input
                          [id]="'sub-' + row.business.id"
                          data-testid="sub-name-input"
                          type="text"
                          [(ngModel)]="draftName"
                          placeholder="e.g. Engineering"
                          (keyup.enter)="createSub(row.business)"
                        />
                        <button class="compact" [disabled]="busy()" (click)="createSub(row.business)">Create sub-business</button>
                        <button class="ghost compact" (click)="closePanel()">Cancel</button>
                      </div>
                    }
                    @case ('rename') {
                      <label [attr.for]="'rename-' + row.business.id">Rename {{ row.business.name }}</label>
                      <div class="row">
                        <input
                          [id]="'rename-' + row.business.id"
                          type="text"
                          [(ngModel)]="draftName"
                          (keyup.enter)="rename(row.business)"
                        />
                        <button class="compact" [disabled]="busy()" (click)="rename(row.business)">Save name</button>
                        <button class="ghost compact" (click)="closePanel()">Cancel</button>
                      </div>
                    }
                    @case ('move') {
                      <label [attr.for]="'move-' + row.business.id">Move {{ row.business.name }} under</label>
                      <div class="row">
                        <select [id]="'move-' + row.business.id" [(ngModel)]="draftTarget">
                          <option value="" disabled>Choose a new parent…</option>
                          @for (t of moveTargets(row.business); track t.id) {
                            <option [value]="t.id">{{ t.label }}</option>
                          }
                        </select>
                        <button class="compact" [disabled]="busy() || !draftTarget" (click)="move(row.business)">Move here</button>
                        <button class="ghost compact" (click)="closePanel()">Cancel</button>
                      </div>
                    }
                  }
                </div>
              }

              @if (confirmDelete()?.id === row.business.id) {
                <div class="panel danger-panel">
                  <span>Delete <b>{{ row.business.name }}</b>? This can't be undone.</span>
                  <div class="row">
                    <button class="danger compact" [disabled]="busy()" (click)="doDelete(row.business)">Confirm delete</button>
                    <button class="ghost compact" (click)="confirmDelete.set(null)">Cancel</button>
                  </div>
                </div>
              }
            </li>
          } @empty {
            <li class="empty">No businesses yet — create your master business below.</li>
          }
        </ul>
      }

      @if (error()) { <p class="msg error">{{ error() }}</p> }
    </section>

    <section class="card" style="margin-top:20px">
      <h2>Create a master business</h2>
      <p class="sub">A master business is the root of its own tenant — fully isolated from your others.</p>
      <form (ngSubmit)="createMaster()">
        <label for="bizname">Business name</label>
        <input id="bizname" type="text" name="name" [(ngModel)]="masterName" placeholder="Acme, Inc." required />
        <button type="submit" [disabled]="busy()">{{ busy() ? 'Working…' : 'Create master business' }}</button>
      </form>
    </section>
  `,
})
export class DashboardComponent implements OnInit {
  private auth = inject(AuthService);
  private api = inject(BusinessService);
  private router = inject(Router);

  profile = signal<Profile | null>(null);
  businesses = signal<Business[]>([]);
  collapsed = signal<ReadonlySet<string>>(new Set());
  loading = signal(true);
  busy = signal(false);
  error = signal('');

  panel = signal<{ id: string; kind: PanelKind } | null>(null);
  confirmDelete = signal<{ id: string } | null>(null);
  draftName = '';
  draftTarget = '';
  masterName = '';

  readonly rows = computed<Row[]>(() => flatten(buildTree(this.businesses()), this.collapsed()));

  ngOnInit(): void {
    this.auth.me().subscribe({ next: (p) => this.profile.set(p), error: () => this.forceLogin() });
    this.loadBusinesses();
  }

  loadBusinesses(): void {
    this.loading.set(true);
    this.api.list().subscribe({
      next: (r) => {
        this.businesses.set(r.items ?? []);
        this.loading.set(false);
      },
      error: () => {
        this.loading.set(false);
        this.error.set('Could not load your businesses. Try refreshing.');
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

  logout(): void {
    this.auth.logout().subscribe({ next: () => this.forceLogin(), error: () => this.forceLogin() });
  }

  private forceLogin(): void {
    void this.router.navigateByUrl('/login');
  }
}
