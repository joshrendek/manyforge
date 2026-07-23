import { HttpErrorResponse } from '@angular/common/http';
import { Component, OnInit, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { ActivatedRoute, RouterLink } from '@angular/router';
import { Board, FeedbackService, IngestKey, Post } from '../../core/feedback.service';
import { PageHeader } from '../../ui/page-header/page-header';
import { Spinner } from '../../ui/spinner/spinner';
import { StatusPill } from '../../ui/status-pill/status-pill';
import { feedbackStatusLabel, feedbackStatusTone } from '../../ui/status';
import { ToastService } from '../../ui/toast/toast.service';

// Board-detail page (/feedback/:businessId/:boardId). Reads businessId + boardId from the
// route (mirrors contact-detail). Loads the board (header + settings), its posts (moderation
// table: status workflow, vote count, convert→ticket, delete), and its publishable ingest
// keys (create/copy/revoke — the key is public, so it is shown, not masked). Each mutating
// action reflects its result into the UI then reloads the affected list, never leaving stale
// state.
@Component({
  selector: 'app-feedback-board-detail',
  imports: [FormsModule, RouterLink, PageHeader, Spinner, StatusPill],
  template: `
    <div class="mf-card" data-testid="board-detail">
      <mf-page-header title="Feedback board">
        <ng-container actions>
          <a
            class="mf-btn mf-btn-ghost mf-btn-sm"
            routerLink="/feedback"
            data-testid="back-to-boards"
            >Back to boards</a
          >
        </ng-container>
      </mf-page-header>

      @if (loading()) {
        <div class="mf-loading-row" data-testid="board-detail-loading">
          <mf-spinner />
          <span>Loading board…</span>
        </div>
      } @else if (error()) {
        <div class="mf-empty-inline">
          <p data-testid="board-detail-error">{{ error() }}</p>
          <button class="mf-btn mf-btn-ghost mf-btn-sm" (click)="reload()">Try again</button>
        </div>
      } @else if (board(); as b) {
        <header class="board-head">
          <h1 class="board-name" data-testid="board-detail-name">{{ b.name }}</h1>
          <p class="board-sub">
            <span class="board-slug">{{ b.slug }}</span>
            @if (b.is_public) {
              <mf-status-pill tone="success" label="Public" />
            } @else {
              <mf-status-pill tone="neutral" label="Private" />
            }
          </p>
        </header>

        <!-- Board settings: name, description, public visibility. -->
        <form class="mf-card block" data-testid="board-settings" (ngSubmit)="saveBoard()">
          <h2 class="block-title">Board settings</h2>
          <div class="mf-field">
            <label for="bd-name">Name</label>
            <input
              id="bd-name"
              class="mf-input"
              type="text"
              name="name"
              data-testid="board-name-input"
              [(ngModel)]="name"
            />
          </div>
          <div class="mf-field">
            <label for="bd-desc">Description</label>
            <input
              id="bd-desc"
              class="mf-input"
              type="text"
              name="desc"
              data-testid="board-desc-input"
              [(ngModel)]="description"
              placeholder="Optional"
            />
          </div>
          <label class="mf-check" for="bd-public">
            <input
              id="bd-public"
              type="checkbox"
              name="public"
              data-testid="board-public-input"
              [(ngModel)]="isPublic"
            />
            Public — accept submissions from the Apple/Android SDK &amp; portal
          </label>
          <div>
            <button
              type="submit"
              class="mf-btn mf-btn-primary mf-btn-sm"
              data-testid="board-save"
              [disabled]="saving()"
            >
              {{ saving() ? 'Saving…' : 'Save settings' }}
            </button>
          </div>
        </form>

        <!-- Ingest keys: publishable client keys embedded in an SDK. Public, so shown + copyable. -->
        <div class="mf-card block" data-testid="board-keys">
          <h2 class="block-title">Publishable SDK keys</h2>
          <p class="mf-hint">
            Embed a key in your iOS/Android app to submit feedback via the public API. Keys are
            publishable (not secrets); revoke one to cut off its access.
          </p>
          <form class="mf-filters" data-testid="key-new" (ngSubmit)="createKey()">
            <div class="mf-field" style="flex:1 1 220px">
              <label for="bd-key-label">Key label</label>
              <input
                id="bd-key-label"
                class="mf-input"
                type="text"
                name="keyLabel"
                placeholder="e.g. iOS app v1"
                [(ngModel)]="newKeyLabel"
              />
            </div>
            <div style="display:flex;align-items:flex-end">
              <button
                type="submit"
                class="mf-btn mf-btn-primary mf-btn-sm"
                data-testid="key-create"
                [disabled]="creatingKey()"
              >
                {{ creatingKey() ? 'Creating…' : 'Create key' }}
              </button>
            </div>
          </form>

          <div class="mf-table" data-testid="keys-list">
            @for (k of keys(); track k.id) {
              <div class="mf-tr" data-testid="key-row" [attr.data-key-id]="k.id">
                <code
                  style="flex:2;overflow:hidden;text-overflow:ellipsis"
                  data-testid="key-value"
                  >{{ k.publishable_key }}</code
                >
                <span style="flex:1">{{ k.label || '—' }}</span>
                <span style="flex:0 0 auto">
                  @if (k.status === 'enabled') {
                    <mf-status-pill tone="success" label="Enabled" />
                  } @else {
                    <mf-status-pill tone="neutral" label="Revoked" />
                  }
                </span>
                <span style="flex:0 0 auto;display:flex;gap:6px">
                  <button
                    type="button"
                    class="mf-btn mf-btn-ghost mf-btn-sm"
                    data-testid="key-copy"
                    (click)="copyKey(k)"
                  >
                    Copy
                  </button>
                  @if (k.status === 'enabled') {
                    <button
                      type="button"
                      class="mf-btn mf-btn-danger mf-btn-sm"
                      data-testid="key-revoke"
                      [disabled]="busyKey() === k.id"
                      (click)="revokeKey(k)"
                    >
                      Revoke
                    </button>
                  }
                </span>
              </div>
            }
            @if (!keys().length) {
              <span class="mf-hint" data-testid="keys-empty"
                >No keys yet. Create one to start collecting SDK feedback.</span
              >
            }
          </div>
        </div>

        <!-- New post (internal submission). -->
        <form class="mf-card block" data-testid="post-new" (ngSubmit)="createPost()">
          <h2 class="block-title">Add a post</h2>
          <div class="mf-field">
            <label for="bd-post-title">Title</label>
            <input
              id="bd-post-title"
              class="mf-input"
              type="text"
              name="postTitle"
              placeholder="What should we build?"
              [(ngModel)]="newPostTitle"
            />
          </div>
          <div class="mf-field">
            <label for="bd-post-body">Details</label>
            <textarea
              id="bd-post-body"
              class="mf-textarea"
              name="postBody"
              rows="2"
              placeholder="Optional"
              [(ngModel)]="newPostBody"
            ></textarea>
          </div>
          <div>
            <button
              type="submit"
              class="mf-btn mf-btn-primary mf-btn-sm"
              data-testid="post-create"
              [disabled]="!newPostTitle.trim() || creatingPost()"
            >
              {{ creatingPost() ? 'Adding…' : 'Add post' }}
            </button>
          </div>
        </form>

        <!-- Posts moderation table. -->
        <div class="mf-card block" data-testid="posts-block">
          <h2 class="block-title">Posts</h2>
          <div class="mf-table" data-testid="posts-list">
            <div class="mf-tr mf-th">
              <span style="flex:0 0 56px">Votes</span>
              <span style="flex:2">Title</span>
              <span style="flex:0 0 150px">Status</span>
              <span style="flex:0 0 auto">Actions</span>
            </div>
            @for (p of posts(); track p.id) {
              <div class="mf-tr" data-testid="post-row" [attr.data-post-id]="p.id">
                <span style="flex:0 0 56px" class="votes" data-testid="post-votes"
                  >▲ {{ p.vote_count }}</span
                >
                <span style="flex:2" data-testid="post-title">
                  {{ p.title }}
                  <mf-status-pill [tone]="tone(p.status)" [label]="label(p.status)" />
                </span>
                <span style="flex:0 0 150px">
                  <select
                    class="mf-select"
                    data-testid="post-status-select"
                    [ngModel]="p.status"
                    [ngModelOptions]="{ standalone: true }"
                    (ngModelChange)="setStatus(p, $event)"
                  >
                    @for (s of statuses; track s) {
                      <option [value]="s">{{ label(s) }}</option>
                    }
                  </select>
                </span>
                <span style="flex:0 0 auto;display:flex;gap:6px">
                  @if (p.ticket_id) {
                    <a
                      class="mf-btn mf-btn-link mf-btn-sm"
                      data-testid="post-ticket-link"
                      [routerLink]="['/support', businessIdSig(), p.ticket_id]"
                      >Ticket ↗</a
                    >
                  } @else {
                    <button
                      type="button"
                      class="mf-btn mf-btn-ghost mf-btn-sm"
                      data-testid="post-convert"
                      [disabled]="busyPost() === p.id"
                      (click)="convert(p)"
                    >
                      Convert to ticket
                    </button>
                  }
                  <button
                    type="button"
                    class="mf-btn mf-btn-danger mf-btn-sm"
                    data-testid="post-delete"
                    [disabled]="busyPost() === p.id"
                    (click)="removePost(p)"
                  >
                    Delete
                  </button>
                </span>
              </div>
            }
            @if (!posts().length) {
              <span class="mf-hint" data-testid="posts-empty">No posts yet.</span>
            }
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
        padding: 18px 0;
        display: flex;
        flex-direction: column;
        align-items: flex-start;
        gap: 12px;
      }
      .board-head {
        margin-bottom: 18px;
      }
      .board-name {
        font-size: var(--mf-fs-xl);
        font-weight: 680;
        letter-spacing: -0.02em;
        margin: 0;
      }
      .board-sub {
        display: flex;
        align-items: center;
        gap: 10px;
        margin: 6px 0 0;
      }
      .board-slug {
        color: var(--mf-text-muted);
        font-size: var(--mf-fs-sm);
        font-family: var(--mf-mono, ui-monospace, monospace);
      }
      .block {
        margin-top: 16px;
        display: flex;
        flex-direction: column;
        gap: 12px;
      }
      .block-title {
        font-size: var(--mf-fs-base);
        font-weight: 640;
        margin: 0;
      }
      .mf-check {
        display: inline-flex;
        align-items: center;
        gap: 8px;
        font-size: var(--mf-fs-sm);
        color: var(--mf-text-muted);
        cursor: pointer;
      }
      .votes {
        font-weight: 640;
        color: var(--mf-accent);
      }
    `,
  ],
})
export class FeedbackBoardDetailComponent implements OnInit {
  private route = inject(ActivatedRoute);
  private feedback = inject(FeedbackService);
  private toast = inject(ToastService);

  private businessId = '';
  private boardId = '';

  readonly statuses = ['open', 'planned', 'in_progress', 'done', 'declined'];

  board = signal<Board | null>(null);
  posts = signal<Post[]>([]);
  keys = signal<IngestKey[]>([]);

  loading = signal(true);
  error = signal('');
  saving = signal(false);
  creatingPost = signal(false);
  creatingKey = signal(false);
  // The id of the post/key currently mid-mutation, so only its buttons disable.
  busyPost = signal('');
  busyKey = signal('');

  // Board-settings + create-form fields.
  name = '';
  description = '';
  isPublic = false;
  newPostTitle = '';
  newPostBody = '';
  newKeyLabel = '';

  tone = feedbackStatusTone;
  label = feedbackStatusLabel;

  businessIdSig(): string {
    return this.businessId;
  }

  ngOnInit(): void {
    this.businessId = this.route.snapshot.paramMap.get('businessId') ?? '';
    this.boardId = this.route.snapshot.paramMap.get('boardId') ?? '';
    this.reload();
    this.reloadPosts();
    this.reloadKeys();
  }

  reload(): void {
    if (!this.businessId || !this.boardId) {
      this.loading.set(false);
      this.error.set("We couldn't load this board.");
      return;
    }
    this.loading.set(true);
    this.error.set('');
    this.feedback.getBoard(this.businessId, this.boardId).subscribe({
      next: (b) => {
        this.board.set(b);
        this.name = b.name;
        this.description = b.description ?? '';
        this.isPublic = b.is_public;
        this.loading.set(false);
      },
      error: (e: HttpErrorResponse) => {
        this.loading.set(false);
        this.error.set(this.describeLoad(e));
      },
    });
  }

  reloadPosts(): void {
    if (!this.businessId || !this.boardId) return;
    this.feedback.listPosts(this.businessId, this.boardId).subscribe({
      next: (r) => this.posts.set(r.items ?? []),
      error: () => {},
    });
  }

  reloadKeys(): void {
    if (!this.businessId || !this.boardId) return;
    this.feedback.listKeys(this.businessId, this.boardId).subscribe({
      next: (r) => this.keys.set(r.items ?? []),
      error: () => {},
    });
  }

  saveBoard(): void {
    if (this.saving()) return;
    this.saving.set(true);
    this.feedback
      .updateBoard(this.businessId, this.boardId, {
        name: this.name.trim(),
        description: this.description.trim(),
        is_public: this.isPublic,
      })
      .subscribe({
        next: (b) => {
          this.board.set(b);
          this.saving.set(false);
          this.toast.success('Board saved');
        },
        error: (e: HttpErrorResponse) => {
          this.saving.set(false);
          this.toast.error(this.describeMutation(e, 'Could not save the board'));
        },
      });
  }

  createPost(): void {
    const title = this.newPostTitle.trim();
    if (!title || this.creatingPost()) return;
    this.creatingPost.set(true);
    const body = this.newPostBody.trim();
    this.feedback
      .createPost(this.businessId, this.boardId, body ? { title, body } : { title })
      .subscribe({
        next: () => {
          this.newPostTitle = '';
          this.newPostBody = '';
          this.creatingPost.set(false);
          this.toast.success('Post added');
          this.reloadPosts();
        },
        error: (e: HttpErrorResponse) => {
          this.creatingPost.set(false);
          this.toast.error(this.describeMutation(e, 'Could not add the post'));
        },
      });
  }

  setStatus(post: Post, status: string): void {
    if (status === post.status) return;
    this.busyPost.set(post.id);
    this.feedback.setPostStatus(this.businessId, post.id, status).subscribe({
      next: (updated) => {
        this.busyPost.set('');
        this.patchPost(updated);
        this.toast.success(`Status → ${this.label(status)}`);
      },
      error: (e: HttpErrorResponse) => {
        this.busyPost.set('');
        this.toast.error(this.describeMutation(e, 'Could not change the status'));
        // Reload so the select reverts to the true server state.
        this.reloadPosts();
      },
    });
  }

  convert(post: Post): void {
    if (this.busyPost()) return;
    this.busyPost.set(post.id);
    this.feedback.convertPost(this.businessId, post.id).subscribe({
      next: (r) => {
        this.busyPost.set('');
        this.patchPost({ ...post, ticket_id: r.ticket_id });
        this.toast.success('Converted to a support ticket');
      },
      error: (e: HttpErrorResponse) => {
        this.busyPost.set('');
        this.toast.error(this.describeMutation(e, 'Could not convert the post'));
      },
    });
  }

  removePost(post: Post): void {
    if (this.busyPost()) return;
    this.busyPost.set(post.id);
    this.feedback.deletePost(this.businessId, post.id).subscribe({
      next: () => {
        this.busyPost.set('');
        this.posts.set(this.posts().filter((p) => p.id !== post.id));
        this.toast.success('Post deleted');
      },
      error: (e: HttpErrorResponse) => {
        this.busyPost.set('');
        this.toast.error(this.describeMutation(e, 'Could not delete the post'));
      },
    });
  }

  createKey(): void {
    if (this.creatingKey()) return;
    this.creatingKey.set(true);
    const label = this.newKeyLabel.trim();
    this.feedback.createKey(this.businessId, this.boardId, label ? { label } : {}).subscribe({
      next: (k) => {
        this.newKeyLabel = '';
        this.creatingKey.set(false);
        this.keys.set([k, ...this.keys()]);
        this.toast.success('Key created');
      },
      error: (e: HttpErrorResponse) => {
        this.creatingKey.set(false);
        this.toast.error(this.describeMutation(e, 'Could not create the key'));
      },
    });
  }

  revokeKey(key: IngestKey): void {
    if (this.busyKey()) return;
    this.busyKey.set(key.id);
    this.feedback.revokeKey(this.businessId, key.id).subscribe({
      next: (k) => {
        this.busyKey.set('');
        this.patchKey(k);
        this.toast.success('Key revoked');
      },
      error: (e: HttpErrorResponse) => {
        this.busyKey.set('');
        this.toast.error(this.describeMutation(e, 'Could not revoke the key'));
      },
    });
  }

  copyKey(key: IngestKey): void {
    void navigator.clipboard?.writeText(key.publishable_key).then(
      () => this.toast.success('Key copied'),
      () => this.toast.error('Could not copy'),
    );
  }

  private patchPost(updated: Post): void {
    this.posts.set(this.posts().map((p) => (p.id === updated.id ? updated : p)));
  }

  private patchKey(updated: IngestKey): void {
    this.keys.set(this.keys().map((k) => (k.id === updated.id ? updated : k)));
  }

  private describeLoad(e: HttpErrorResponse): string {
    if (e.status === 403 || e.status === 404) return "You don't have access to do that.";
    return "We couldn't load this board.";
  }

  private describeMutation(e: HttpErrorResponse, fallback: string): string {
    if (e.status === 400) {
      const msg = (e.error as { message?: string } | null)?.message;
      return msg
        ? `That change was rejected: ${msg}`
        : 'That change was rejected. Check your input.';
    }
    if (e.status === 409) return 'That conflicts with the current state.';
    if (e.status === 403 || e.status === 404) return "You don't have access to do that.";
    return fallback;
  }
}
