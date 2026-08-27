import { DatePipe } from '@angular/common';
import { Component, OnInit, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { Router, RouterLink } from '@angular/router';
import { BusinessService } from '../../core/business.service';
import { CurrentBusinessService } from '../../core/current-business.service';
import { MailingService, MailingTemplate } from '../../core/mailing.service';
import { Business } from '../../core/tree';
import { EmptyState } from '../../ui/empty-state/empty-state';
import { PageHeader } from '../../ui/page-header/page-header';
import { Spinner } from '../../ui/spinner/spinner';
import { ToastService } from '../../ui/toast/toast.service';

@Component({
  selector: 'app-mailing-templates-list',
  standalone: true,
  imports: [DatePipe, FormsModule, RouterLink, EmptyState, PageHeader, Spinner],
  template: `
    <div class="mf-card" data-testid="mailing-templates-page">
      <mf-page-header
        title="Email templates"
        subtitle="Write reusable campaign content in Markdown"
      >
        <a
          routerLink="/mailing/lists"
          class="mf-btn mf-btn-ghost mf-btn-sm"
          data-testid="mailing-lists-link"
          actions
          >Lists</a
        >
      </mf-page-header>
      <div class="mf-filters">
        <div class="mf-field grow">
          <label for="template-business">Business</label
          ><select
            id="template-business"
            class="mf-select"
            data-testid="business-select"
            [ngModel]="businessId()"
            (ngModelChange)="selectBusiness($event)"
          >
            <option value="" disabled>Choose a business…</option>
            @for (business of businesses(); track business.id) {
              <option [value]="business.id">{{ business.name }}</option>
            }
          </select>
        </div>
        @if (loading()) {
          <mf-spinner />
        }
      </div>
      @if (businessId()) {
        <form class="mf-filters" data-testid="mailing-template-new" (ngSubmit)="create()">
          <div class="mf-field grow">
            <label for="template-name">Template name</label
            ><input
              id="template-name"
              class="mf-input"
              name="name"
              data-testid="mailing-template-name"
              [(ngModel)]="newName"
            />
          </div>
          <div class="mf-field grow">
            <label for="template-subject">Subject</label
            ><input
              id="template-subject"
              class="mf-input"
              name="subject"
              data-testid="mailing-template-subject"
              [(ngModel)]="newSubject"
            />
          </div>
          <button
            type="submit"
            class="mf-btn mf-btn-primary mf-btn-sm"
            data-testid="mailing-template-create"
            [disabled]="!newName.trim() || !newSubject.trim() || creating()"
          >
            {{ creating() ? 'Creating…' : 'Create template' }}
          </button>
        </form>
      }
      <div class="mf-table" data-testid="mailing-templates-table">
        <div class="mf-tr mf-th"><span>Name</span><span>Subject</span><span>Updated</span></div>
        @for (template of items(); track template.id) {
          <div class="mf-tr" data-testid="mailing-template-row">
            <span
              ><a
                [routerLink]="['/mailing', businessId(), 'templates', template.id]"
                data-testid="mailing-template-open"
                >{{ template.name }}</a
              ></span
            ><span>{{ template.subject }}</span
            ><span>{{ template.updated_at | date: 'mediumDate' }}</span>
          </div>
        }
        @if (!items().length && businessId() && !loading()) {
          <mf-empty-state title="No templates yet" data-testid="mailing-templates-empty"
            >Create one above.</mf-empty-state
          >
        }
      </div>
      @if (nextCursor()) {
        <button
          type="button"
          class="mf-btn mf-btn-ghost mf-btn-sm more"
          data-testid="mailing-templates-load-more"
          (click)="loadMore()"
        >
          Load more
        </button>
      }
      @if (error()) {
        <p class="mf-err" data-testid="mailing-templates-error">{{ error() }}</p>
      }
    </div>
  `,
  styles: [
    `
      .grow {
        flex: 1 1 240px;
      }
      .mf-tr > span {
        flex: 1;
      }
      .mf-filters .mf-btn {
        align-self: end;
        min-height: 36px;
      }
      .more {
        margin-top: 12px;
      }
    `,
  ],
})
export class MailingTemplatesListComponent implements OnInit {
  private businessesApi = inject(BusinessService);
  private mailing = inject(MailingService);
  private current = inject(CurrentBusinessService);
  private router = inject(Router);
  private toast = inject(ToastService);

  businesses = signal<Business[]>([]);
  businessId = signal('');
  items = signal<MailingTemplate[]>([]);
  nextCursor = signal<string | null>(null);
  loading = signal(false);
  creating = signal(false);
  error = signal('');
  newName = '';
  newSubject = '';

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
    this.reload();
  }
  reload(): void {
    this.items.set([]);
    this.nextCursor.set(null);
    this.load();
  }
  loadMore(): void {
    if (this.nextCursor()) this.load(this.nextCursor()!);
  }
  private load(cursor?: string): void {
    const id = this.businessId();
    if (!id || this.loading()) return;
    this.loading.set(true);
    this.mailing.listTemplates(id, cursor).subscribe({
      next: (page) => {
        if (id !== this.businessId()) return;
        this.items.update((items) =>
          cursor ? [...items, ...(page.items ?? [])] : (page.items ?? []),
        );
        this.nextCursor.set(page.next_cursor ?? null);
        this.loading.set(false);
        this.error.set('');
      },
      error: () => {
        this.loading.set(false);
        this.error.set('Could not load templates');
      },
    });
  }
  create(): void {
    const name = this.newName.trim();
    const subject = this.newSubject.trim();
    if (!name || !subject || this.creating()) return;
    this.creating.set(true);
    this.mailing
      .createTemplate(this.businessId(), {
        name,
        subject,
        body_markdown: '',
        track_opens: true,
        track_clicks: true,
      })
      .subscribe({
        next: (template) => {
          this.creating.set(false);
          this.toast.success('Template created');
          void this.router.navigate(['/mailing', this.businessId(), 'templates', template.id]);
        },
        error: () => {
          this.creating.set(false);
          this.toast.error('Could not create template');
        },
      });
  }
}
