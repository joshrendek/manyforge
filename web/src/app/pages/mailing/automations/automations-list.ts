import { DatePipe } from '@angular/common';
import { Component, OnInit, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { Router, RouterLink } from '@angular/router';
import { Automation, AutomationsService } from '../../../core/automations.service';
import { BusinessService } from '../../../core/business.service';
import { CurrentBusinessService } from '../../../core/current-business.service';
import { Business } from '../../../core/tree';
import { EmptyState } from '../../../ui/empty-state/empty-state';
import { PageHeader } from '../../../ui/page-header/page-header';
import { Spinner } from '../../../ui/spinner/spinner';
import { StatusPill } from '../../../ui/status-pill/status-pill';
import { automationStatusTone } from '../../../ui/status';
import { ToastService } from '../../../ui/toast/toast.service';

@Component({
  selector: 'app-automations-list',
  standalone: true,
  imports: [DatePipe, FormsModule, RouterLink, EmptyState, PageHeader, Spinner, StatusPill],
  template: `
    <div class="mf-card" data-testid="automations-page">
      <mf-page-header title="Automations" subtitle="Build branching journeys that react to subscriber activity">
        <a routerLink="/mailing/lists" class="mf-btn mf-btn-ghost mf-btn-sm" data-testid="automations-lists-link" actions>Lists</a>
        <a routerLink="/mailing/templates" class="mf-btn mf-btn-ghost mf-btn-sm" data-testid="automations-templates-link" actions>Templates</a>
      </mf-page-header>
      <div class="mf-filters">
        <div class="mf-field grow"><label for="automation-business">Business</label><select id="automation-business" class="mf-select" data-testid="business-select" [ngModel]="businessId()" (ngModelChange)="selectBusiness($event)"><option value="" disabled>Choose a business…</option>@for (business of businesses(); track business.id) { <option [value]="business.id">{{ business.name }}</option> }</select></div>
        @if (loading()) { <span class="loading"><mf-spinner /> Loading automations…</span> }
      </div>
      @if (businessId()) {
        <form class="mf-filters" data-testid="automation-new" (ngSubmit)="create()">
          <div class="mf-field grow"><label for="automation-name">Automation name</label><input id="automation-name" class="mf-input" name="automationName" data-testid="automation-name" [(ngModel)]="newName" placeholder="Welcome journey" /></div>
          <label class="reenroll"><input type="checkbox" name="allowReenroll" data-testid="automation-reenroll" [(ngModel)]="allowReenroll" /> Allow subscribers to re-enroll</label>
          <button type="submit" class="mf-btn mf-btn-primary mf-btn-sm" data-testid="automation-create" [disabled]="!newName.trim() || creating()">{{ creating() ? 'Creating…' : 'Create automation' }}</button>
        </form>
      }
      <div class="mf-table" data-testid="automations-table">
        <div class="mf-tr mf-th"><span class="wide">Automation</span><span>Status</span><span>Re-enrollment</span><span>Updated</span></div>
        @for (automation of items(); track automation.id) {
          <div class="mf-tr" data-testid="automation-row"><span class="wide"><a [routerLink]="['/mailing', businessId(), 'automations', automation.id]" data-testid="automation-open">{{ automation.name }}</a></span><span><mf-status-pill [tone]="statusTone(automation.status)" [label]="automation.status" /></span><span>{{ automation.allow_reenroll ? 'Allowed' : 'Once only' }}</span><span>{{ automation.updated_at | date: 'medium' }}</span></div>
        }
        @if (!items().length && businessId() && !loading()) { <mf-empty-state title="No automations yet" data-testid="automations-empty">Create a draft above.</mf-empty-state> }
      </div>
      @if (nextCursor()) { <button type="button" class="mf-btn mf-btn-ghost mf-btn-sm more" data-testid="automations-load-more" [disabled]="loading()" (click)="loadMore()">Load more</button> }
      @if (error()) { <p class="mf-err" data-testid="automations-error">{{ error() }}</p> }
    </div>
  `,
  styles: [`
    .grow,.wide{flex:2}.mf-tr>span:not(.wide){flex:1}.loading,.reenroll{display:flex;align-items:center;gap:8px;color:var(--mf-text-muted);font-size:var(--mf-fs-sm)}.mf-filters .mf-btn{align-self:end;min-height:36px}.reenroll{align-self:end;min-height:36px}.more{margin-top:12px}
  `],
})
export class AutomationsListComponent implements OnInit {
  private readonly businessesApi = inject(BusinessService);
  private readonly automations = inject(AutomationsService);
  private readonly current = inject(CurrentBusinessService);
  private readonly router = inject(Router);
  private readonly toast = inject(ToastService);
  private loadSeq = 0;

  businesses = signal<Business[]>([]);
  businessId = signal('');
  items = signal<Automation[]>([]);
  nextCursor = signal<string | null>(null);
  loading = signal(false);
  creating = signal(false);
  error = signal('');
  newName = '';
  allowReenroll = false;
  readonly statusTone = automationStatusTone;

  ngOnInit(): void {
    this.businessesApi.list().subscribe({
      next: (page) => {
        const items = page.items ?? [];
        this.businesses.set(items);
        const id = this.current.businessId() ?? items[0]?.id;
        if (id) this.selectBusiness(id);
      },
      error: () => this.error.set('Could not load businesses'),
    });
  }
  selectBusiness(id: string): void {
    this.businessId.set(id);
    this.current.set(id);
    this.loading.set(false);
    this.items.set([]);
    this.nextCursor.set(null);
    this.load();
  }
  loadMore(): void { if (this.nextCursor()) this.load(this.nextCursor()!); }
  create(): void {
    const name = this.newName.trim();
    const businessId = this.businessId();
    if (!name || !businessId || this.creating()) return;
    this.creating.set(true);
    this.automations.create(businessId, { name, allow_reenroll: this.allowReenroll }).subscribe({
      next: (automation) => {
        this.creating.set(false);
        this.toast.success('Automation created');
        void this.router.navigate(['/mailing', businessId, 'automations', automation.id]);
      },
      error: () => { this.creating.set(false); this.toast.error('Could not create automation'); },
    });
  }
  private load(cursor?: string): void {
    const businessId = this.businessId();
    if (!businessId || this.loading()) return;
    const seq = ++this.loadSeq;
    this.loading.set(true);
    this.automations.list(businessId, cursor).subscribe({
      next: (page) => {
        if (seq !== this.loadSeq || businessId !== this.businessId()) return;
        this.items.update((items) => cursor ? [...items, ...(page.items ?? [])] : (page.items ?? []));
        this.nextCursor.set(page.next_cursor ?? null);
        this.loading.set(false);
        this.error.set('');
      },
      error: () => { if (seq === this.loadSeq) { this.loading.set(false); this.error.set('Could not load automations'); } },
    });
  }
}
