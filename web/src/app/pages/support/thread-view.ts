import { Component, OnInit, inject, signal } from '@angular/core';
import { HttpErrorResponse } from '@angular/common/http';
import { DatePipe } from '@angular/common';
import { FormsModule } from '@angular/forms';
import { ActivatedRoute, RouterLink } from '@angular/router';
import {
  PatchTicket,
  Ticket,
  TicketMessage,
  TicketPriority,
  TicketService,
  TicketStatus,
} from '../../core/ticket.service';
import { AuthService, Profile } from '../../core/auth.service';

// Thread view for a single ticket. Mirrors signup.ts's signal-driven view
// switching and dashboard.ts's load/error pattern. Business id + ticket id come
// from the route (/support/:businessId/:tid), seeded by the ticket list. Renders
// the ticket header + embedded requester + the ordered message thread with
// inbound/outbound/note styling, attachments, and the SPF/DKIM/DMARC flags.
@Component({
  selector: 'app-thread-view',
  imports: [RouterLink, DatePipe, FormsModule],
  template: `
    <section class="card">
      <div class="spread">
        <a class="linklike" routerLink="/support" data-testid="back-to-list">Back to tickets</a>
      </div>

      @if (loading()) {
        <p class="empty">Loading conversation…</p>
      } @else if (loadFailed()) {
        <div class="empty">
          <p>{{ error() || "We couldn't load this conversation." }}</p>
          <button class="ghost compact" (click)="reload()">Try again</button>
        </div>
      } @else if (ticket(); as t) {
        <header class="thread-head" data-testid="thread-header">
          <h1 data-testid="thread-subject">{{ t.subject || '(no subject)' }}</h1>
          <div class="thread-tags">
            <span class="badge" data-testid="thread-status">{{ t.status }}</span>
            <span class="pill" data-testid="thread-priority">{{ t.priority }}</span>
            @for (tag of t.tags; track tag) {
              <span class="badge" data-testid="thread-tag">{{ tag }}</span>
            }
            @if (!t.assignee_principal_id) {
              <span class="badge" data-testid="thread-unassigned">unassigned</span>
            }
          </div>
          <p class="requester" data-testid="thread-requester">
            <b>{{ t.requester.display_name || t.requester.email }}</b>
            <span class="muted">&lt;{{ t.requester.email }}&gt;</span>
          </p>

          <!-- US3 triage controls. Each mutation PATCHes the ticket and reflects
               the returned Ticket so the header above never goes stale. -->
          <div class="triage" data-testid="triage">
            <label class="triage-field">
              <span class="triage-label">Status</span>
              <select
                data-testid="triage-status"
                [disabled]="triaging()"
                [ngModel]="t.status"
                (ngModelChange)="changeStatus($event)"
              >
                @for (s of statuses; track s) {
                  <option [value]="s">{{ s }}</option>
                }
              </select>
            </label>

            <label class="triage-field">
              <span class="triage-label">Priority</span>
              <select
                data-testid="triage-priority"
                [disabled]="triaging()"
                [ngModel]="t.priority"
                (ngModelChange)="changePriority($event)"
              >
                @for (p of priorities; track p) {
                  <option [value]="p">{{ p }}</option>
                }
              </select>
            </label>

            <div class="triage-field triage-tags">
              <span class="triage-label">Tags</span>
              <div class="chips" data-testid="triage-tags">
                @for (tag of t.tags; track tag) {
                  <span class="chip" data-testid="triage-chip">
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
                  class="chip-input"
                  data-testid="triage-tag-input"
                  placeholder="add tag…"
                  [(ngModel)]="tagDraft"
                  [disabled]="triaging()"
                  (keyup.enter)="addTag()"
                />
              </div>
            </div>

            <div class="triage-field triage-assignee">
              <span class="triage-label">Assignee</span>
              <div class="assignee-row" data-testid="triage-assignee">
                <button
                  type="button"
                  class="ghost compact"
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
                  class="ghost compact"
                  data-testid="unassign"
                  [disabled]="triaging() || !t.assignee_principal_id"
                  (click)="unassign()"
                >
                  Unassign
                </button>
              </div>
              <!-- No "list assignable members" endpoint exists yet (see follow-up),
                   so assigning anyone other than yourself is a manual principal-uuid
                   entry. The backend validates eligibility + tickets.assign. -->
              <div class="assignee-manual">
                <input
                  type="text"
                  class="chip-input"
                  data-testid="assign-uuid-input"
                  placeholder="principal uuid…"
                  [(ngModel)]="assigneeDraft"
                  [disabled]="triaging()"
                  (keyup.enter)="assignManual()"
                />
                <button
                  type="button"
                  class="ghost compact"
                  data-testid="assign-uuid-submit"
                  [disabled]="triaging() || !assigneeDraft.trim()"
                  (click)="assignManual()"
                >
                  Assign
                </button>
              </div>
            </div>

            @if (triageError()) {
              <p class="msg error" data-testid="triage-error">{{ triageError() }}</p>
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
            <li class="empty" data-testid="message-empty">No messages in this conversation yet.</li>
          }
        </ul>

        @if (nextCursor()) {
          <button
            class="ghost compact"
            data-testid="load-more-messages"
            [disabled]="busy()"
            (click)="loadMore()"
          >
            {{ busy() ? 'Loading…' : 'Load earlier messages' }}
          </button>
        }

        <div class="composer" data-testid="composer">
          <div class="composer-toggle" data-testid="composer-toggle">
            <button
              class="toggle-btn"
              data-testid="toggle-reply"
              [class.active]="!noteMode()"
              [attr.aria-pressed]="!noteMode()"
              (click)="noteMode.set(false)"
            >
              Reply
            </button>
            <button
              class="toggle-btn"
              data-testid="toggle-note"
              [class.active]="noteMode()"
              [attr.aria-pressed]="noteMode()"
              (click)="noteMode.set(true)"
            >
              Internal note
            </button>
          </div>
          <textarea
            class="composer-body"
            data-testid="composer-body"
            [placeholder]="noteMode() ? 'Add an internal note…' : 'Write a reply…'"
            [(ngModel)]="composerText"
            [disabled]="sending()"
            rows="4"
          ></textarea>
          @if (sendError()) {
            <p class="msg error" data-testid="composer-error">{{ sendError() }}</p>
          }
          <div class="composer-actions">
            <button
              class="primary compact"
              data-testid="composer-submit"
              [disabled]="!composerText.trim() || sending()"
              (click)="submitComposer()"
            >
              {{ sending() ? 'Sending…' : noteMode() ? 'Add note' : 'Send reply' }}
            </button>
          </div>
        </div>
      }
    </section>
  `,
  styles: [
    `
      .thread-head {
        margin-bottom: 18px;
      }
      .thread-tags {
        display: flex;
        gap: 8px;
        flex-wrap: wrap;
        margin: 8px 0;
      }
      .thread-head .requester {
        color: var(--muted);
        font-size: 13px;
        margin: 6px 0 0;
      }
      .thread-head .requester b {
        color: var(--text);
      }

      .triage {
        margin-top: 14px;
        padding-top: 14px;
        border-top: 1px solid var(--border);
        display: flex;
        flex-wrap: wrap;
        gap: 16px;
        align-items: flex-start;
      }
      .triage-field {
        display: flex;
        flex-direction: column;
        gap: 4px;
      }
      .triage-label {
        font-size: 11px;
        font-weight: 600;
        text-transform: uppercase;
        letter-spacing: 0.04em;
        color: var(--muted);
      }
      .triage select {
        padding: 6px 8px;
        border: 1px solid var(--border);
        border-radius: var(--radius-sm);
        background: var(--panel);
        color: var(--text);
        font-size: 13px;
        font-family: inherit;
      }
      .triage select:disabled {
        opacity: 0.6;
        cursor: not-allowed;
      }
      .chips {
        display: flex;
        flex-wrap: wrap;
        gap: 6px;
        align-items: center;
      }
      .chip {
        display: inline-flex;
        align-items: center;
        gap: 4px;
        padding: 2px 4px 2px 8px;
        font-size: 12px;
        border: 1px solid var(--border);
        border-radius: 999px;
        background: var(--panel-2);
      }
      .chip-x {
        border: none;
        background: none;
        cursor: pointer;
        color: var(--muted);
        font-size: 14px;
        line-height: 1;
        padding: 0 2px;
      }
      .chip-x:disabled {
        cursor: not-allowed;
        opacity: 0.5;
      }
      .chip-input {
        min-width: 96px;
        padding: 4px 8px;
        border: 1px solid var(--border);
        border-radius: var(--radius-sm);
        background: var(--panel);
        color: var(--text);
        font-size: 13px;
        font-family: inherit;
      }
      .assignee-row {
        display: flex;
        gap: 6px;
      }
      .assignee-manual {
        display: flex;
        gap: 6px;
        margin-top: 6px;
      }
      .triage .msg.error {
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
        border: 1px solid var(--border);
        border-radius: var(--radius-sm);
        padding: 14px 16px;
        background: var(--panel-2);
      }
      .message.inbound {
        box-shadow: inset 3px 0 0 var(--accent-soft);
      }
      .message.outbound {
        box-shadow: inset 3px 0 0 var(--ok);
        background: var(--panel);
      }
      .message.note {
        box-shadow: inset 3px 0 0 var(--danger-soft);
        background: var(--danger-soft);
      }

      .message-head {
        display: flex;
        justify-content: space-between;
        align-items: baseline;
        gap: 12px;
      }
      .message-head .direction {
        font-weight: 600;
        font-size: 12.5px;
        text-transform: uppercase;
        letter-spacing: 0.03em;
        color: var(--muted);
      }
      .message-head .when {
        color: var(--faint);
        font-size: 12px;
      }
      .message-body {
        margin-top: 8px;
        white-space: pre-wrap;
        font-size: 14px;
        line-height: 1.5;
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
        font-size: 12.5px;
      }
      .attachment .filename {
        font-weight: 600;
      }

      .auth-flags {
        display: flex;
        gap: 8px;
        flex-wrap: wrap;
        margin-top: 10px;
      }
      .auth-flags .flag {
        font-size: 10.5px;
        font-weight: 600;
        letter-spacing: 0.02em;
        text-transform: uppercase;
        padding: 3px 8px;
        border-radius: 999px;
        border: 1px solid var(--border);
        color: var(--muted);
      }
      .auth-flags .flag.pass {
        color: var(--ok);
        border-color: var(--ok);
      }
      .auth-flags .flag.fail {
        color: var(--danger);
        border-color: var(--danger-dim);
      }

      [data-testid='load-more-messages'] {
        margin-top: 16px;
      }

      .delivery-failed {
        margin-top: 8px;
        font-size: 11px;
        font-weight: 600;
        text-transform: uppercase;
        letter-spacing: 0.04em;
        color: var(--danger);
        padding: 2px 8px;
        border: 1px solid var(--danger-dim);
        border-radius: 999px;
        display: inline-block;
      }

      .composer {
        margin-top: 24px;
        border-top: 1px solid var(--border);
        padding-top: 16px;
        display: grid;
        gap: 10px;
      }
      .composer-toggle {
        display: flex;
        gap: 0;
        border: 1px solid var(--border);
        border-radius: var(--radius-sm);
        overflow: hidden;
        width: fit-content;
      }
      .toggle-btn {
        background: var(--panel-2);
        border: none;
        padding: 6px 14px;
        font-size: 13px;
        font-weight: 500;
        cursor: pointer;
        color: var(--muted);
        transition: background 0.1s;
      }
      .toggle-btn:not(:last-child) {
        border-right: 1px solid var(--border);
      }
      .toggle-btn.active {
        background: var(--accent-soft);
        color: var(--text);
      }
      .composer-body {
        width: 100%;
        box-sizing: border-box;
        resize: vertical;
        font-size: 14px;
        line-height: 1.5;
        padding: 10px 12px;
        border: 1px solid var(--border);
        border-radius: var(--radius-sm);
        background: var(--panel);
        color: var(--text);
        font-family: inherit;
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

  private businessId = '';
  private ticketId = '';

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

  ngOnInit(): void {
    this.businessId = this.route.snapshot.paramMap.get('businessId') ?? '';
    this.ticketId = this.route.snapshot.paramMap.get('tid') ?? '';
    // Best-effort: drives "Assign to me". A failure just hides that button.
    this.auth.me().subscribe({ next: (p) => this.profile.set(p), error: () => {} });
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
}
