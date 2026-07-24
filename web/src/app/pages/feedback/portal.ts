import { HttpErrorResponse } from '@angular/common/http';
import { Component, OnInit, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { ActivatedRoute } from '@angular/router';
import { PublicFeedbackService, PublicPost } from '../../core/public-feedback.service';
import { StatusPill } from '../../ui/status-pill/status-pill';
import { ThemeToggle } from '../../ui/theme-toggle/theme-toggle';
import { feedbackStatusLabel, feedbackStatusTone } from '../../ui/status';
import { ToastService } from '../../ui/toast/toast.service';

// Public, UNAUTHENTICATED feedback portal (/p/:key). The web equivalent of the mobile SDK:
// anyone with the link can read public posts, submit a feature request, and upvote — no login.
// Identity is an anonymous per-browser id (localStorage) so a person can vote once per post;
// the server enforces one-vote-per-identity via that id. A 401 (unknown/revoked key or a
// private board) collapses to a single "unavailable" state — no existence oracle.
@Component({
  selector: 'app-feedback-portal',
  imports: [FormsModule, StatusPill, ThemeToggle],
  template: `
    <div class="portal" data-testid="portal">
      <header class="portal-bar">
        <span class="brand">Feedback</span>
        <span style="flex:1"></span>
        <mf-theme-toggle />
      </header>

      @if (unavailable()) {
        <div class="portal-body">
          <div class="mf-card unavailable" data-testid="portal-unavailable">
            <h1>This feedback board isn’t available</h1>
            <p class="mf-hint">
              The link may be inactive, or the board is no longer public. Check with whoever shared
              it.
            </p>
          </div>
        </div>
      } @else {
        <div class="portal-body">
          <div class="intro">
            <h1 data-testid="portal-heading">Share your feedback</h1>
            <p class="mf-hint">
              Suggest an idea or upvote what matters most to you. Top-voted items rise to the top.
            </p>
          </div>

          <!-- Submit a new idea. -->
          <form class="mf-card submit-card" data-testid="portal-submit" (ngSubmit)="submit()">
            <div class="mf-field">
              <label for="pt-title">Your idea</label>
              <input
                id="pt-title"
                class="mf-input"
                type="text"
                name="title"
                data-testid="portal-title-input"
                placeholder="e.g. Add dark mode"
                [(ngModel)]="title"
                maxlength="300"
              />
            </div>
            <div class="mf-field">
              <label for="pt-body">Details (optional)</label>
              <textarea
                id="pt-body"
                class="mf-textarea"
                name="body"
                rows="2"
                data-testid="portal-body-input"
                placeholder="What problem would this solve?"
                [(ngModel)]="body"
              ></textarea>
            </div>
            <div>
              <button
                type="submit"
                class="mf-btn mf-btn-primary"
                data-testid="portal-submit-btn"
                [disabled]="!title.trim() || submitting()"
              >
                {{ submitting() ? 'Submitting…' : 'Submit feedback' }}
              </button>
            </div>
          </form>

          <!-- Ranked list of public posts. -->
          @if (loading()) {
            <p class="mf-hint" data-testid="portal-loading">Loading…</p>
          } @else if (!posts().length) {
            <p class="mf-hint" data-testid="portal-empty">
              No ideas yet — be the first to suggest one!
            </p>
          } @else {
            <ul class="post-list" data-testid="portal-posts">
              @for (p of posts(); track p.id) {
                <li class="post" data-testid="portal-post">
                  <button
                    type="button"
                    class="vote"
                    data-testid="portal-upvote"
                    [class.voted]="voted().has(p.id)"
                    [disabled]="voting().has(p.id)"
                    [attr.aria-label]="'Upvote ' + p.title"
                    (click)="upvote(p)"
                  >
                    <span class="caret">▲</span>
                    <span class="count" data-testid="portal-vote-count">{{ p.vote_count }}</span>
                  </button>
                  <div class="post-main">
                    <div class="post-head">
                      <span class="post-title" data-testid="portal-post-title">{{ p.title }}</span>
                      <mf-status-pill [tone]="tone(p.status)" [label]="label(p.status)" />
                    </div>
                    @if (p.body) {
                      <p class="post-body">{{ p.body }}</p>
                    }
                  </div>
                </li>
              }
            </ul>
          }
        </div>
      }

      <footer class="portal-foot">
        <span class="mf-hint">Powered by <b>ManyForge</b></span>
      </footer>
    </div>
  `,
  styles: [
    `
      .portal {
        min-height: 100vh;
        display: flex;
        flex-direction: column;
        background: var(--mf-bg, var(--mf-surface));
      }
      .portal-bar {
        display: flex;
        align-items: center;
        gap: 12px;
        padding: 14px 20px;
        border-bottom: 1px solid var(--mf-border);
      }
      .portal-bar .brand {
        font-weight: 680;
        letter-spacing: -0.01em;
      }
      .portal-body {
        width: 100%;
        max-width: 680px;
        margin: 0 auto;
        padding: 28px 20px 48px;
        display: flex;
        flex-direction: column;
        gap: 18px;
        flex: 1;
      }
      .intro h1 {
        font-size: var(--mf-fs-xl);
        font-weight: 680;
        letter-spacing: -0.02em;
        margin: 0 0 6px;
      }
      .submit-card {
        display: flex;
        flex-direction: column;
        gap: 12px;
      }
      .unavailable {
        margin: 60px auto;
        text-align: center;
      }
      .unavailable h1 {
        font-size: var(--mf-fs-lg);
        font-weight: 660;
        margin: 0 0 8px;
      }
      .post-list {
        list-style: none;
        margin: 0;
        padding: 0;
        display: flex;
        flex-direction: column;
        gap: 10px;
      }
      .post {
        display: flex;
        gap: 14px;
        align-items: flex-start;
        background: var(--mf-card, var(--mf-surface));
        border: 1px solid var(--mf-border);
        border-radius: var(--mf-radius);
        padding: 14px 16px;
      }
      .vote {
        display: flex;
        flex-direction: column;
        align-items: center;
        gap: 2px;
        min-width: 52px;
        padding: 8px 0;
        border: 1px solid var(--mf-border);
        border-radius: var(--mf-radius);
        background: transparent;
        color: var(--mf-text-muted);
        cursor: pointer;
        font-weight: 640;
        transition:
          border-color 0.12s,
          color 0.12s;
      }
      .vote:hover:not(:disabled) {
        border-color: var(--mf-accent);
        color: var(--mf-accent);
      }
      .vote.voted {
        border-color: var(--mf-accent);
        color: var(--mf-accent);
        background: var(--mf-accent-soft, transparent);
      }
      .vote .caret {
        font-size: 12px;
        line-height: 1;
      }
      .vote .count {
        font-size: var(--mf-fs-base);
      }
      .post-main {
        flex: 1;
        min-width: 0;
      }
      .post-head {
        display: flex;
        align-items: center;
        gap: 10px;
        flex-wrap: wrap;
      }
      .post-title {
        font-weight: 620;
      }
      .post-body {
        color: var(--mf-text-muted);
        font-size: var(--mf-fs-sm);
        margin: 6px 0 0;
      }
      .portal-foot {
        text-align: center;
        padding: 20px;
        border-top: 1px solid var(--mf-border);
      }
    `,
  ],
})
export class FeedbackPortalComponent implements OnInit {
  private route = inject(ActivatedRoute);
  private pub = inject(PublicFeedbackService);
  private toast = inject(ToastService);

  private key = '';
  private deviceId = '';
  private votedStorageKey = '';

  posts = signal<PublicPost[]>([]);
  loading = signal(true);
  unavailable = signal(false);
  submitting = signal(false);
  voting = signal<Set<string>>(new Set());
  voted = signal<Set<string>>(new Set());

  title = '';
  body = '';

  tone = feedbackStatusTone;
  label = feedbackStatusLabel;

  ngOnInit(): void {
    this.key = this.route.snapshot.paramMap.get('key') ?? '';
    this.deviceId = this.ensureDeviceId();
    this.votedStorageKey = `mf_fb_voted_${this.key}`;
    this.voted.set(new Set(this.loadVoted()));
    this.load();
  }

  load(): void {
    if (!this.key) {
      this.loading.set(false);
      this.unavailable.set(true);
      return;
    }
    this.loading.set(true);
    this.pub.listPosts(this.key).subscribe({
      next: (r) => {
        this.posts.set(r.items ?? []);
        this.unavailable.set(false);
        this.loading.set(false);
      },
      error: () => {
        // 401 (unknown/revoked key or private board) and any other failure collapse to one
        // "unavailable" state — no existence oracle, no bounce to /login.
        this.posts.set([]);
        this.unavailable.set(true);
        this.loading.set(false);
      },
    });
  }

  submit(): void {
    const title = this.title.trim();
    if (!title || this.submitting()) return;
    this.submitting.set(true);
    const body = this.body.trim();
    this.pub
      .submit(this.key, { title, body: body || undefined, author_identity: this.deviceId })
      .subscribe({
        next: () => {
          this.title = '';
          this.body = '';
          this.submitting.set(false);
          this.toast.success('Thanks! Your feedback was submitted.');
          this.load();
        },
        error: (e: HttpErrorResponse) => {
          this.submitting.set(false);
          this.toast.error(
            e.status === 400 ? 'Please enter a title.' : 'Could not submit right now.',
          );
        },
      });
  }

  upvote(post: PublicPost): void {
    if (this.voting().has(post.id)) return;
    this.setVoting(post.id, true);
    this.pub.vote(this.key, post.id, this.deviceId).subscribe({
      next: (r) => {
        this.setVoting(post.id, false);
        this.patchCount(post.id, r.vote_count);
        this.markVoted(post.id);
        if (r.voted) this.toast.success('Thanks for your vote!');
      },
      error: () => {
        this.setVoting(post.id, false);
        this.toast.error('Could not record your vote');
      },
    });
  }

  private ensureDeviceId(): string {
    let id = localStorage.getItem('mf_fb_device');
    if (!id) {
      id =
        typeof crypto !== 'undefined' && crypto.randomUUID
          ? crypto.randomUUID()
          : 'dev-' + Math.random().toString(36).slice(2) + Date.now().toString(36);
      localStorage.setItem('mf_fb_device', id);
    }
    return id;
  }

  private loadVoted(): string[] {
    try {
      const raw = localStorage.getItem(this.votedStorageKey);
      return raw ? (JSON.parse(raw) as string[]) : [];
    } catch {
      return [];
    }
  }

  private markVoted(id: string): void {
    const v = new Set(this.voted());
    v.add(id);
    this.voted.set(v);
    localStorage.setItem(this.votedStorageKey, JSON.stringify([...v]));
  }

  private setVoting(id: string, on: boolean): void {
    const v = new Set(this.voting());
    if (on) v.add(id);
    else v.delete(id);
    this.voting.set(v);
  }

  private patchCount(id: string, count: number): void {
    this.posts.set(this.posts().map((p) => (p.id === id ? { ...p, vote_count: count } : p)));
  }
}
