import { Component, OnInit, inject, signal } from '@angular/core';
import { HttpErrorResponse } from '@angular/common/http';
import { DatePipe } from '@angular/common';
import { FormsModule } from '@angular/forms';
import { ActivatedRoute, RouterLink } from '@angular/router';
import {
  AssignableMember,
  PatchTicket,
  Ticket,
  TicketMessage,
  TicketPriority,
  TicketService,
  TicketStatus,
} from '../../core/ticket.service';
import { AuthService, Profile } from '../../core/auth.service';
import { Agent, AgentsService } from '../../core/agents.service';
import { ToastService } from '../../ui/toast/toast.service';
import { PageHeader } from '../../ui/page-header/page-header';
import { StatusPill } from '../../ui/status-pill/status-pill';
import { Spinner } from '../../ui/spinner/spinner';
import { ticketStatusTone, ticketPriorityTone } from '../../ui/status';

// Thread view for a single ticket. Mirrors signup.ts's signal-driven view
// switching and dashboard.ts's load/error pattern. Business id + ticket id come
// from the route (/support/:businessId/:tid), seeded by the ticket list. Renders
// the ticket header + embedded requester + the ordered message thread with
// inbound/outbound/note styling, attachments, and the SPF/DKIM/DMARC flags.
@Component({
  selector: 'app-thread-view',
  imports: [RouterLink, DatePipe, FormsModule, PageHeader, StatusPill, Spinner],
  template: `
    <div class="mf-card">
      <mf-page-header title="Conversation">
        <ng-container actions>
          <a class="mf-btn mf-btn-ghost mf-btn-sm" routerLink="/support" data-testid="back-to-list"
            >Back to tickets</a
          >
        </ng-container>
      </mf-page-header>

      @if (loading()) {
        <div class="mf-loading-row">
          <mf-spinner />
          <span>Loading conversation…</span>
        </div>
      } @else if (loadFailed()) {
        <div class="mf-empty-inline">
          <p>{{ error() || "We couldn't load this conversation." }}</p>
          <button class="mf-btn mf-btn-ghost mf-btn-sm" (click)="reload()">Try again</button>
        </div>
      } @else if (ticket(); as t) {
        <header class="thread-head" data-testid="thread-header">
          <h1 class="thread-subject" data-testid="thread-subject">
            {{ t.subject || '(no subject)' }}
          </h1>
          <div class="thread-tags">
            <mf-status-pill
              [tone]="ticketStatusTone(t.status)"
              [label]="t.status"
              data-testid="thread-status"
            />
            <mf-status-pill
              [tone]="ticketPriorityTone(t.priority)"
              [label]="t.priority"
              data-testid="thread-priority"
            />
            @for (tag of t.tags; track tag) {
              <span class="mf-pill mf-pill-neutral" data-testid="thread-tag">{{ tag }}</span>
            }
            @if (!t.assignee_principal_id) {
              <span class="mf-pill mf-pill-neutral" data-testid="thread-unassigned"
                >unassigned</span
              >
            }
          </div>
          <p class="requester" data-testid="thread-requester">
            <b>{{ t.requester.display_name || t.requester.email }}</b>
            <span class="muted">&lt;{{ t.requester.email }}&gt;</span>
          </p>

          <!-- US3 triage controls. Each mutation PATCHes the ticket and reflects
               the returned Ticket so the header above never goes stale. -->
          <div class="triage mf-card" data-testid="triage">
            <div class="mf-field triage-field">
              <label>Status</label>
              <select
                class="mf-select"
                data-testid="triage-status"
                [disabled]="triaging()"
                [ngModel]="t.status"
                (ngModelChange)="changeStatus($event)"
              >
                @for (s of statuses; track s) {
                  <option [value]="s">{{ s }}</option>
                }
              </select>
            </div>

            <div class="mf-field triage-field">
              <label>Priority</label>
              <select
                class="mf-select"
                data-testid="triage-priority"
                [disabled]="triaging()"
                [ngModel]="t.priority"
                (ngModelChange)="changePriority($event)"
              >
                @for (p of priorities; track p) {
                  <option [value]="p">{{ p }}</option>
                }
              </select>
            </div>

            <div class="mf-field triage-field triage-tags-field">
              <label>Tags</label>
              <div class="chips" data-testid="triage-tags">
                @for (tag of t.tags; track tag) {
                  <span class="mf-pill mf-pill-neutral chip" data-testid="triage-chip">
                    {{ tag }}
                    <button
                      type="button"
                      class="chip-x"
                      data-testid="triage-chip-remove"
                      [attr.aria-label]="'Remove tag ' + tag"
                      [disabled]="triaging()"
                      (click)="removeTag(tag)"
                    >
                      ×
                    </button>
                  </span>
                }
                <input
                  type="text"
                  class="mf-input chip-input"
                  data-testid="triage-tag-input"
                  placeholder="add tag…"
                  [(ngModel)]="tagDraft"
                  [disabled]="triaging()"
                  (keyup.enter)="addTag()"
                />
              </div>
            </div>

            <div class="mf-field triage-field triage-assignee-field">
              <label>Assignee</label>
              <div class="assignee-row" data-testid="triage-assignee">
                <button
                  type="button"
                  class="mf-btn mf-btn-ghost mf-btn-sm"
                  data-testid="assign-to-me"
                  [disabled]="
                    triaging() || !myPrincipalId() || t.assignee_principal_id === myPrincipalId()
                  "
                  (click)="assignToMe()"
                >
                  Assign to me
                </button>
                <button
                  type="button"
                  class="mf-btn mf-btn-ghost mf-btn-sm"
                  data-testid="unassign"
                  [disabled]="triaging() || !t.assignee_principal_id"
                  (click)="unassign()"
                >
                  Unassign
                </button>
              </div>
              <!-- The picker lists the business's assignable members (FR-011); shown
                   only when the list loads (the caller has tickets.assign and members
                   exist). Selecting (unassigned) clears the assignee. -->
              @if (members().length > 0) {
                <div class="assignee-picker-row">
                  <select
                    class="mf-select assignee-picker"
                    data-testid="assignee-picker"
                    aria-label="Assign to a member"
                    [disabled]="triaging()"
                    [ngModel]="t.assignee_principal_id ?? ''"
                    (ngModelChange)="assignPicked($event)"
                  >
                    <option value="">Unassigned</option>
                    @for (m of members(); track m.id) {
                      <option [value]="m.id">{{ m.display_name }} ({{ m.email }})</option>
                    }
                  </select>
                </div>
              }
              <!-- Manual principal-uuid fallback for an ancestor member or one beyond
                   the picker's capped page. The backend validates eligibility +
                   tickets.assign. -->
              <div class="assignee-manual">
                <input
                  type="text"
                  class="mf-input chip-input"
                  data-testid="assign-uuid-input"
                  placeholder="principal uuid…"
                  [(ngModel)]="assigneeDraft"
                  [disabled]="triaging()"
                  (keyup.enter)="assignManual()"
                />
                <button
                  type="button"
                  class="mf-btn mf-btn-ghost mf-btn-sm"
                  data-testid="assign-uuid-submit"
                  [disabled]="triaging() || !assigneeDraft.trim()"
                  (click)="assignManual()"
                >
                  Assign
                </button>
              </div>
            </div>

            <div class="mf-field triage-field">
              <label>Run agent</label>
              @if (enabledAgents().length === 0) {
                <span class="mf-hint" data-testid="run-agent-none"
                  >No enabled agents for this business.</span
                >
              } @else {
                <div class="assignee-row" data-testid="run-agent-control">
                  @if (enabledAgents().length > 1) {
                    <select
                      class="mf-select"
                      data-testid="run-agent-select"
                      aria-label="Choose an agent"
                      [disabled]="running()"
                      [ngModel]="selectedAgentId()"
                      (ngModelChange)="selectedAgentId.set($event)"
                    >
                      @for (a of enabledAgents(); track a.id) {
                        <option [value]="a.id">{{ a.name }}</option>
                      }
                    </select>
                  }
                  <button
                    type="button"
                    class="mf-btn mf-btn-primary mf-btn-sm"
                    data-testid="run-agent-btn"
                    [disabled]="running() || !selectedAgentId()"
                    (click)="runAgent()"
                  >
                    {{ running() ? 'Starting…' : 'Run agent' }}
                  </button>
                </div>
              }
            </div>

            @if (triageError()) {
              <p class="mf-err triage-error" data-testid="triage-error">{{ triageError() }}</p>
            }
          </div>
        </header>

        <ul class="thread" data-testid="message-thread">
          @for (m of messages(); track m.id) {
            <li
              class="message"
              data-testid="message"
              [attr.data-direction]="m.direction"
              [class.inbound]="m.direction === 'inbound'"
              [class.outbound]="m.direction === 'outbound'"
              [class.note]="m.direction === 'note'"
            >
              <div class="message-head">
                <span class="direction" data-testid="message-direction">
                  @switch (m.direction) {
                    @case ('inbound') {
                      Received
                    }
                    @case ('outbound') {
                      Reply
                    }
                    @case ('note') {
                      Internal note
                    }
                  }
                </span>
                <span class="when">{{ m.created_at | date: 'medium' }}</span>
              </div>

              <div class="message-body" data-testid="message-body">
                {{ m.body_text || '(no text body)' }}
              </div>

              @if (m.delivery_state === 'failed') {
                <div class="delivery-failed" data-testid="delivery-failed">Failed to send</div>
              }

              @if (m.attachments.length) {
                <ul class="attachments" data-testid="message-attachments">
                  @for (a of m.attachments; track a.id) {
                    <li class="attachment" data-testid="attachment">
                      <span class="filename">{{ a.filename }}</span>
                      <span class="muted">{{ a.content_type }} · {{ a.size }} bytes</span>
                    </li>
                  }
                </ul>
              }

              @if (m.direction === 'inbound') {
                <div class="auth-flags" data-testid="auth-flags">
                  <span class="flag" [class]="m.spf_result" data-testid="spf-result"
                    >SPF: {{ m.spf_result }}</span
                  >
                  <span class="flag" [class]="m.dkim_result" data-testid="dkim-result"
                    >DKIM: {{ m.dkim_result }}</span
                  >
                  <span class="flag" [class]="m.dmarc_result" data-testid="dmarc-result"
                    >DMARC: {{ m.dmarc_result }}</span
                  >
                </div>
              }
            </li>
          } @empty {
            <li class="message-empty" data-testid="message-empty">
              No messages in this conversation yet.
            </li>
          }
        </ul>

        @if (nextCursor()) {
          <button
            class="mf-btn mf-btn-ghost load-more"
            data-testid="load-more-messages"
            [disabled]="busy()"
            (click)="loadMore()"
          >
            {{ busy() ? 'Loading…' : 'Load earlier messages' }}
          </button>
        }

        <div class="composer mf-card" data-testid="composer">
          <div class="composer-toggle" data-testid="composer-toggle">
            <button
              class="mf-btn mf-btn-sm toggle-btn"
              [class.mf-btn-primary]="!noteMode()"
              [class.mf-btn-ghost]="noteMode()"
              data-testid="toggle-reply"
              [attr.aria-pressed]="!noteMode()"
              (click)="noteMode.set(false)"
            >
              Reply
            </button>
            <button
              class="mf-btn mf-btn-sm toggle-btn"
              [class.mf-btn-primary]="noteMode()"
              [class.mf-btn-ghost]="!noteMode()"
              data-testid="toggle-note"
              [attr.aria-pressed]="noteMode()"
              (click)="noteMode.set(true)"
            >
              Internal note
            </button>
          </div>
          <textarea
            class="mf-textarea composer-body"
            data-testid="composer-body"
            [placeholder]="noteMode() ? 'Add an internal note…' : 'Write a reply…'"
            [(ngModel)]="composerText"
            [disabled]="sending()"
            rows="4"
          ></textarea>
          @if (sendError()) {
            <p class="mf-err" data-testid="composer-error">{{ sendError() }}</p>
          }
          <div class="composer-actions">
            <button
              class="mf-btn mf-btn-primary"
              data-testid="composer-submit"
              [disabled]="!composerText.trim() || sending()"
              (click)="submitComposer()"
            >
              {{ sending() ? 'Sending…' : noteMode() ? 'Add note' : 'Send reply' }}
            </button>
          </div>
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
        color: var(--mf-text-muted);
        font-size: var(--mf-fs-sm);
        padding: 18px 0;
      }
      .mf-empty-inline {
        color: var(--mf-text-muted);
        font-size: var(--mf-fs-base);
        padding: 18px 0;
        display: flex;
        flex-direction: column;
        align-items: flex-start;
        gap: 12px;
      }

      .thread-head {
        margin-bottom: 18px;
      }
      .thread-subject {
        font-size: var(--mf-fs-xl);
        font-weight: 680;
        letter-spacing: -0.02em;
        margin: 0;
      }
      .thread-tags {
        display: flex;
        gap: 8px;
        flex-wrap: wrap;
        margin: 8px 0;
        align-items: center;
      }
      .thread-head .requester {
        color: var(--mf-text-muted);
        font-size: var(--mf-fs-sm);
        margin: 6px 0 0;
      }
      .thread-head .requester b {
        color: var(--mf-text);
      }

      .triage {
        margin-top: 14px;
        display: flex;
        flex-wrap: wrap;
        gap: 16px;
        align-items: flex-start;
      }
      .triage-field {
        flex: 0 0 auto;
      }
      .triage-field label {
        font-size: var(--mf-fs-xs);
        font-weight: 600;
        text-transform: uppercase;
        letter-spacing: 0.04em;
        color: var(--mf-text-muted);
      }
      .triage .mf-select {
        width: auto;
        min-width: 130px;
      }
      .triage .mf-select:disabled {
        opacity: 0.6;
        cursor: not-allowed;
      }
      .triage-tags-field,
      .triage-assignee-field {
        flex: 1 1 240px;
      }
      .chips {
        display: flex;
        flex-wrap: wrap;
        gap: 6px;
        align-items: center;
      }
      .chip {
        text-transform: none;
        letter-spacing: normal;
        font-weight: 500;
        padding: 3px 4px 3px 9px;
      }
      .chip-x {
        border: none;
        background: none;
        cursor: pointer;
        color: var(--mf-text-muted);
        font-size: 14px;
        line-height: 1;
        padding: 0 2px;
      }
      .chip-x:disabled {
        cursor: not-allowed;
        opacity: 0.5;
      }
      .chip-input {
        width: auto;
        min-width: 96px;
        padding: 4px 8px;
        font-size: var(--mf-fs-sm);
      }
      .assignee-row {
        display: flex;
        gap: 6px;
      }
      .assignee-picker-row {
        margin-top: 6px;
      }
      .assignee-picker {
        width: auto;
        min-width: 200px;
      }
      .assignee-manual {
        display: flex;
        gap: 6px;
        margin-top: 6px;
      }
      .triage-error {
        flex-basis: 100%;
        margin: 0;
      }

      .thread {
        list-style: none;
        padding: 0;
        margin: 0;
        display: grid;
        gap: 12px;
      }
      .message {
        border: 1px solid var(--mf-border);
        border-radius: var(--mf-radius-sm);
        padding: 14px 16px;
        background: var(--mf-surface-2);
      }
      .message.inbound {
        box-shadow: inset 3px 0 0 var(--mf-accent);
      }
      .message.outbound {
        box-shadow: inset 3px 0 0 var(--mf-success);
        background: var(--mf-surface);
      }
      .message.note {
        box-shadow: inset 3px 0 0 var(--mf-danger);
        background: var(--mf-danger-soft);
      }

      .message-head {
        display: flex;
        justify-content: space-between;
        align-items: baseline;
        gap: 12px;
      }
      .message-head .direction {
        font-weight: 600;
        font-size: var(--mf-fs-xs);
        text-transform: uppercase;
        letter-spacing: 0.03em;
        color: var(--mf-text-muted);
      }
      .message-head .when {
        color: var(--mf-text-faint);
        font-size: var(--mf-fs-xs);
      }
      .message-body {
        margin-top: 8px;
        white-space: pre-wrap;
        font-size: var(--mf-fs-base);
        line-height: 1.5;
        color: var(--mf-text);
      }

      .attachments {
        list-style: none;
        padding: 0;
        margin: 10px 0 0;
        display: grid;
        gap: 6px;
      }
      .attachment {
        display: flex;
        gap: 10px;
        align-items: baseline;
        font-size: var(--mf-fs-sm);
        color: var(--mf-text);
      }
      .attachment .filename {
        font-weight: 600;
      }
      .muted {
        color: var(--mf-text-muted);
      }

      .auth-flags {
        display: flex;
        gap: 8px;
        flex-wrap: wrap;
        margin-top: 10px;
      }
      .auth-flags .flag {
        font-size: var(--mf-fs-xs);
        font-weight: 600;
        letter-spacing: 0.02em;
        text-transform: uppercase;
        padding: 3px 8px;
        border-radius: var(--mf-radius-pill);
        border: 1px solid var(--mf-border);
        color: var(--mf-text-muted);
      }
      .auth-flags .flag.pass {
        color: var(--mf-success-text);
        border-color: var(--mf-success);
      }
      .auth-flags .flag.fail {
        color: var(--mf-danger-text);
        border-color: var(--mf-danger);
      }

      .message-empty {
        list-style: none;
        color: var(--mf-text-muted);
        font-size: var(--mf-fs-base);
        padding: 18px 0;
        text-align: center;
      }

      .load-more {
        margin-top: 16px;
      }

      .delivery-failed {
        margin-top: 8px;
        font-size: var(--mf-fs-xs);
        font-weight: 600;
        text-transform: uppercase;
        letter-spacing: 0.04em;
        color: var(--mf-danger-text);
        padding: 2px 8px;
        border: 1px solid var(--mf-danger);
        border-radius: var(--mf-radius-pill);
        display: inline-block;
      }

      .composer {
        margin-top: 24px;
        display: grid;
        gap: 10px;
      }
      .composer-toggle {
        display: flex;
        gap: 6px;
        width: fit-content;
      }
      .composer-body {
        resize: vertical;
        line-height: 1.5;
      }
      .composer-body:disabled {
        opacity: 0.6;
        cursor: not-allowed;
      }
      .composer-actions {
        display: flex;
        justify-content: flex-end;
      }
    `,
  ],
})
export class ThreadViewComponent implements OnInit {
  private route = inject(ActivatedRoute);
  private api = inject(TicketService);
  private auth = inject(AuthService);
  private agents = inject(AgentsService);
  private toast = inject(ToastService);

  private businessId = '';
  private ticketId = '';

  // Run-agent control (manual trigger against this ticket).
  enabledAgents = signal<Agent[]>([]);
  selectedAgentId = signal<string>('');
  running = signal(false);

  ticket = signal<Ticket | null>(null);
  messages = signal<TicketMessage[]>([]);
  nextCursor = signal<string | null>(null);
  loading = signal(true);
  loadFailed = signal(false);
  busy = signal(false);
  error = signal('');

  // Composer state (US2)
  noteMode = signal(false);
  composerText = '';
  sending = signal(false);
  sendError = signal('');

  // Triage state (US3). The full enum lists drive the status/priority dropdowns.
  readonly statuses: TicketStatus[] = ['new', 'open', 'pending', 'solved', 'closed'];
  readonly priorities: TicketPriority[] = ['low', 'normal', 'high', 'urgent'];
  triaging = signal(false);
  triageError = signal('');
  tagDraft = '';
  assigneeDraft = '';

  // The signed-in profile. Spec 001's identity model uses a single principal id
  // per account: the JWT subject IS the principal id, and `/me` returns it as
  // Profile.id — the SAME id used for ticket.assignee_principal_id. So
  // Profile.id is the caller's own principal id, which makes "Assign to me" a
  // real, correct operation (no faking). Permissions (e.g. tickets.assign) are
  // NOT exposed to the client, so the assignee control is always shown and we
  // rely on the backend (404 when the caller lacks tickets.assign).
  profile = signal<Profile | null>(null);
  readonly myPrincipalId = (): string | null => this.profile()?.id ?? null;

  // The business's assignable members (FR-011), loaded best-effort to populate the
  // assignee picker. Stays empty (picker hidden) when the caller lacks tickets.assign
  // (404) or the load fails — the manual-uuid fallback still works.
  members = signal<AssignableMember[]>([]);

  ngOnInit(): void {
    this.businessId = this.route.snapshot.paramMap.get('businessId') ?? '';
    this.ticketId = this.route.snapshot.paramMap.get('tid') ?? '';
    // Best-effort: drives "Assign to me". A failure just hides that button.
    this.auth.me().subscribe({ next: (p) => this.profile.set(p), error: () => {} });
    // Best-effort: populates the assignee picker. A 404 (no tickets.assign) or any
    // failure just leaves the picker hidden.
    if (this.businessId) {
      this.api.listAssignableMembers(this.businessId).subscribe({
        next: (p) => this.members.set(p.items ?? []),
        error: () => {},
      });
      // Best-effort: populates the run-agent control with the business's enabled
      // agents. A failure just leaves the control showing the no-agents hint.
      this.agents.list(this.businessId).subscribe({
        next: (r) => {
          const enabled = (r.items ?? []).filter((a) => a.enabled);
          this.enabledAgents.set(enabled);
          if (enabled.length > 0) this.selectedAgentId.set(enabled[0].id);
        },
        error: () => {},
      });
    }
    this.reload();
  }

  reload(): void {
    if (!this.businessId || !this.ticketId) {
      this.loading.set(false);
      this.loadFailed.set(true);
      return;
    }
    this.loading.set(true);
    this.loadFailed.set(false);
    this.error.set('');
    this.api.getTicket(this.businessId, this.ticketId).subscribe({
      next: (t) => {
        this.ticket.set(t);
        this.loadMessages();
      },
      error: (e: HttpErrorResponse) => {
        this.loading.set(false);
        this.loadFailed.set(true);
        this.error.set(this.describeError(e));
      },
    });
  }

  private loadMessages(): void {
    this.api.listMessages(this.businessId, this.ticketId).subscribe({
      next: (page) => {
        this.messages.set(page.items ?? []);
        this.nextCursor.set(page.next_cursor);
        this.loading.set(false);
      },
      error: (e: HttpErrorResponse) => {
        this.loading.set(false);
        this.loadFailed.set(true);
        this.error.set(this.describeError(e));
      },
    });
  }

  loadMore(): void {
    const cursor = this.nextCursor();
    if (!cursor || this.busy()) return;
    this.busy.set(true);
    this.api.listMessages(this.businessId, this.ticketId, cursor).subscribe({
      next: (page) => {
        this.messages.update((cur) => [...cur, ...(page.items ?? [])]);
        this.nextCursor.set(page.next_cursor);
        this.busy.set(false);
      },
      error: () => this.busy.set(false),
    });
  }

  // Submit reply or note from the composer (US2).
  submitComposer(): void {
    const text = this.composerText.trim();
    if (!text || this.sending()) return;
    this.sending.set(true);
    this.sendError.set('');
    const req$ = this.noteMode()
      ? this.api.addNote(this.businessId, this.ticketId, { body_text: text })
      : this.api.reply(this.businessId, this.ticketId, { body_text: text });
    req$.subscribe({
      next: (msg) => {
        this.messages.update((cur) => [...cur, msg]);
        this.composerText = '';
        this.sending.set(false);
      },
      error: () => {
        this.sendError.set('Failed to send. Please try again.');
        this.sending.set(false);
      },
    });
  }

  // ── Triage (US3) ──────────────────────────────────────────────────────────

  changeStatus(status: TicketStatus): void {
    const t = this.ticket();
    if (!t || status === t.status) return;
    this.patch({ status });
  }

  changePriority(priority: TicketPriority): void {
    const t = this.ticket();
    if (!t || priority === t.priority) return;
    this.patch({ priority });
  }

  // Add a free-text tag. Sends the FULL resulting set (replacement semantics).
  // Local dedupe is a nicety; the backend is the source of truth (citext-folds,
  // rejects empty/whitespace with 400 — surfaced via patch()).
  addTag(): void {
    const t = this.ticket();
    const tag = this.tagDraft.trim();
    if (!t || !tag) return;
    if (t.tags.some((x) => x.toLowerCase() === tag.toLowerCase())) {
      this.tagDraft = '';
      return;
    }
    this.patch({ tags: [...t.tags, tag] }, () => (this.tagDraft = ''));
  }

  removeTag(tag: string): void {
    const t = this.ticket();
    if (!t) return;
    this.patch({ tags: t.tags.filter((x) => x !== tag) });
  }

  // Assignee tri-state. We only ever put `assignee_principal_id` in the body
  // when actually changing it; `patch()` forwards the object verbatim so an
  // omitted key stays omitted (status/priority/tag changes never touch it).
  assignToMe(): void {
    const me = this.myPrincipalId();
    const t = this.ticket();
    if (!me || !t || t.assignee_principal_id === me) return;
    this.patch({ assignee_principal_id: me });
  }

  unassign(): void {
    const t = this.ticket();
    if (!t || !t.assignee_principal_id) return;
    this.patch({ assignee_principal_id: null });
  }

  assignManual(): void {
    const id = this.assigneeDraft.trim();
    if (!id) return;
    this.patch({ assignee_principal_id: id }, () => (this.assigneeDraft = ''));
  }

  // Picker selection: an empty value clears the assignee (null); otherwise assign the
  // chosen member. No-op when it already matches the current assignee, so reselecting
  // the same option fires no redundant PATCH.
  assignPicked(id: string): void {
    const t = this.ticket();
    if (!t) return;
    const next = id === '' ? null : id;
    if (next === (t.assignee_principal_id ?? null)) return;
    this.patch({ assignee_principal_id: next });
  }

  // ── Run agent ──────────────────────────────────────────────────────────────

  // Fire-and-forget: an immediate toast, then an outcome toast when the (synchronous)
  // run returns. The running flag only guards against a double-click before the first
  // response lands. Actions auto-apply or land in /approvals per the agent's autonomy mode.
  runAgent(): void {
    const agentId = this.selectedAgentId();
    if (this.running() || !agentId) return;
    this.running.set(true);
    this.toast.success('Agent started — it will act on this ticket in the background.');
    this.agents
      .run(this.businessId, agentId, { target_type: 'ticket', target_id: this.ticketId })
      .subscribe({
        next: (run) => {
          this.running.set(false);
          if (run.status === 'awaiting_approval') {
            this.toast.success('Agent finished — proposed actions are waiting in Approvals.');
          } else if (run.status === 'failed') {
            this.toast.error('Agent run failed.');
          } else {
            this.toast.success('Agent finished.');
          }
        },
        error: (e: HttpErrorResponse) => {
          this.running.set(false);
          this.toast.error(
            e.status === 403 || e.status === 404
              ? "You don't have access to run agents."
              : 'Could not start the agent run.',
          );
        },
      });
  }

  // Central triage mutation: PATCH, then reflect the returned Ticket (no stale
  // UI) and surface 400/404/409 gracefully without crashing.
  private patch(body: PatchTicket, onOk?: () => void): void {
    if (this.triaging()) return;
    this.triaging.set(true);
    this.triageError.set('');
    this.api.patchTicket(this.businessId, this.ticketId, body).subscribe({
      next: (updated) => {
        this.ticket.set(updated);
        this.triaging.set(false);
        onOk?.();
      },
      error: (e: HttpErrorResponse) => {
        this.triaging.set(false);
        this.triageError.set(this.describeTriageError(e));
      },
    });
  }

  // Triage-specific error copy. 400 carries safe validation detail (e.g. an
  // empty/whitespace tag); 404 covers unknown ticket AND missing tickets.assign
  // (no oracle); 409 is an ineligible assignee / invalid status transition.
  private describeTriageError(e: HttpErrorResponse): string {
    if (e.status === 400) {
      const msg = (e.error as { message?: string } | null)?.message;
      return msg
        ? `That change was rejected: ${msg}`
        : 'That change was rejected. Check your input.';
    }
    if (e.status === 409) return "That change conflicts with the ticket's current state.";
    if (e.status === 403 || e.status === 404) return "You don't have access to do that.";
    return 'Could not update the ticket. Please try again.';
  }

  // No-oracle: 403/404 both map to a generic message (mirrors dashboard.ts).
  private describeError(e: HttpErrorResponse): string {
    if (e.status === 403 || e.status === 404) return "You don't have access to do that.";
    return "We couldn't load this conversation.";
  }

  // Template helpers — delegate to pure status functions so the template stays clean.
  readonly ticketStatusTone = ticketStatusTone;
  readonly ticketPriorityTone = ticketPriorityTone;
}
