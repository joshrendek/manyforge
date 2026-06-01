import { Component, OnInit, inject, signal } from '@angular/core';
import { HttpErrorResponse } from '@angular/common/http';
import { DatePipe } from '@angular/common';
import { FormsModule } from '@angular/forms';
import { ActivatedRoute, RouterLink } from '@angular/router';
import { Ticket, TicketMessage, TicketService } from '../../core/ticket.service';

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
              {{ sending() ? 'Sending…' : (noteMode() ? 'Add note' : 'Send reply') }}
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

  ngOnInit(): void {
    this.businessId = this.route.snapshot.paramMap.get('businessId') ?? '';
    this.ticketId = this.route.snapshot.paramMap.get('tid') ?? '';
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

  // No-oracle: 403/404 both map to a generic message (mirrors dashboard.ts).
  private describeError(e: HttpErrorResponse): string {
    if (e.status === 403 || e.status === 404) return "You don't have access to do that.";
    return "We couldn't load this conversation.";
  }
}
