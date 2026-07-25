import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { ActivatedRoute, provideRouter } from '@angular/router';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { Board, IngestKey, Post } from '../../core/feedback.service';
import { ToastService } from '../../ui/toast/toast.service';
import { FeedbackBoardDetailComponent } from './board-detail';

const biz = 'b1';
const boardId = 'bd1';

function makeBoard(over: Partial<Board> = {}): Board {
  return {
    id: boardId,
    business_id: biz,
    tenant_root_id: 'root',
    slug: 'mobile-app',
    name: 'Mobile App',
    description: 'desc',
    is_public: true,
    created_at: '',
    updated_at: '',
    ...over,
  };
}
function makePost(over: Partial<Post> = {}): Post {
  return {
    id: 'p1',
    business_id: biz,
    tenant_root_id: 'root',
    board_id: boardId,
    title: 'Face ID',
    body: null,
    status: 'open',
    vote_count: 3,
    author_kind: 'public',
    author_principal_id: null,
    author_identity: 'device-1',
    ticket_id: null,
    identity_verified: false,
    created_at: '',
    updated_at: '',
    ...over,
  };
}
function makeKey(over: Partial<IngestKey> = {}): IngestKey {
  return {
    id: 'k1',
    business_id: biz,
    tenant_root_id: 'root',
    board_id: boardId,
    publishable_key: 'fbk_abc',
    label: 'iOS',
    status: 'enabled',
    has_secret: false,
    created_at: '',
    revoked_at: null,
    ...over,
  };
}

describe('FeedbackBoardDetailComponent', () => {
  let fixture: ComponentFixture<FeedbackBoardDetailComponent>;
  let cmp: FeedbackBoardDetailComponent;
  let mock: HttpTestingController;

  function load(
    board = makeBoard(),
    posts: Post[] = [makePost()],
    keys: IngestKey[] = [makeKey()],
  ): void {
    fixture = TestBed.createComponent(FeedbackBoardDetailComponent);
    cmp = fixture.componentInstance;
    fixture.detectChanges(); // ngOnInit → getBoard + listPosts + listKeys
    mock.expectOne(`/api/v1/businesses/${biz}/feedback/boards/${boardId}`).flush(board);
    mock
      .expectOne(`/api/v1/businesses/${biz}/feedback/boards/${boardId}/posts`)
      .flush({ items: posts, next_cursor: null });
    mock
      .expectOne(`/api/v1/businesses/${biz}/feedback/boards/${boardId}/keys`)
      .flush({ items: keys });
    fixture.detectChanges();
  }

  function q(sel: string): HTMLElement | null {
    return fixture.nativeElement.querySelector(sel) as HTMLElement | null;
  }

  beforeEach(() => {
    TestBed.configureTestingModule({
      providers: [
        provideHttpClient(),
        provideHttpClientTesting(),
        provideRouter([]),
        {
          provide: ActivatedRoute,
          useValue: {
            snapshot: {
              paramMap: new Map([
                ['businessId', biz],
                ['boardId', boardId],
              ]),
            },
          },
        },
      ],
    });
    mock = TestBed.inject(HttpTestingController);
    document.documentElement.setAttribute('data-theme', 'light');
  });
  afterEach(() => mock.verify());

  it('loads the board header, posts, and keys', () => {
    load();
    expect(q('[data-testid="board-detail-name"]')?.textContent).toContain('Mobile App');
    expect(q('[data-testid="post-row"]')).toBeTruthy();
    expect(q('[data-testid="post-votes"]')?.textContent).toContain('3');
    expect(q('[data-testid="key-value"]')?.textContent).toContain('fbk_abc');
  });

  it('moderates a post status via PATCH and patches the row in place', () => {
    load();
    cmp.setStatus(makePost(), 'planned');
    const req = mock.expectOne(`/api/v1/businesses/${biz}/feedback/posts/p1`);
    expect(req.request.method).toBe('PATCH');
    expect(req.request.body).toEqual({ status: 'planned' });
    req.flush(makePost({ status: 'planned' }));
    fixture.detectChanges();
    expect(cmp.posts()[0].status).toBe('planned');
  });

  it('converts a post to a ticket and links it in place', () => {
    load();
    const toast = TestBed.inject(ToastService);
    cmp.convert(makePost());
    const req = mock.expectOne(`/api/v1/businesses/${biz}/feedback/posts/p1/convert`);
    expect(req.request.method).toBe('POST');
    req.flush({ ticket_id: 't-99' });
    fixture.detectChanges();
    expect(cmp.posts()[0].ticket_id).toBe('t-99');
    expect(q('[data-testid="post-ticket-link"]')).toBeTruthy();
    expect(toast.toasts().some((t) => t.message.includes('ticket'))).toBe(true);
  });

  it('revokes an ingest key and reflects the revoked status', () => {
    load();
    cmp.revokeKey(makeKey());
    const req = mock.expectOne(`/api/v1/businesses/${biz}/feedback/keys/k1/revoke`);
    expect(req.request.method).toBe('POST');
    req.flush(makeKey({ status: 'revoked', revoked_at: 'now' }));
    fixture.detectChanges();
    expect(cmp.keys()[0].status).toBe('revoked');
    expect(q('[data-testid="keys-list"]')?.textContent).toContain('Revoked');
  });

  it('creates an ingest key and prepends it to the list', () => {
    load(makeBoard(), [], []);
    cmp.newKeyLabel = 'Android';
    cmp.createKey();
    const req = mock.expectOne(`/api/v1/businesses/${biz}/feedback/boards/${boardId}/keys`);
    expect(req.request.body).toEqual({ label: 'Android' });
    req.flush(makeKey({ id: 'k2', label: 'Android', publishable_key: 'fbk_xyz' }));
    fixture.detectChanges();
    expect(cmp.keys()[0].publishable_key).toBe('fbk_xyz');
  });

  it('shows the one-time secret after create, then removes it (and does not re-fetch it) on dismiss', () => {
    load(makeBoard(), [], []);
    expect(q('[data-testid="key-secret-once"]')).toBeFalsy();
    cmp.createKey();
    const req = mock.expectOne(`/api/v1/businesses/${biz}/feedback/boards/${boardId}/keys`);
    req.flush(makeKey({ id: 'k2', has_secret: true, secret: 'fbs_x' }));
    fixture.detectChanges();

    expect(cmp.createdSecret()).toBe('fbs_x');
    expect(q('[data-testid="key-secret-once"]')).toBeTruthy();
    expect(q('[data-testid="key-secret-value"]')?.textContent).toContain('fbs_x');

    cmp.dismissSecret();
    fixture.detectChanges();

    expect(cmp.createdSecret()).toBeNull();
    expect(q('[data-testid="key-secret-once"]')).toBeFalsy();
    expect(fixture.nativeElement.textContent).not.toContain('fbs_x');
    mock.verify(); // no re-fetch of the key/secret happened on dismiss
  });

  it('announces the one-time secret panel via a live region when it appears', () => {
    load(makeBoard(), [], []);
    cmp.createKey();
    mock
      .expectOne(`/api/v1/businesses/${biz}/feedback/boards/${boardId}/keys`)
      .flush(makeKey({ id: 'k2', has_secret: true, secret: 'fbs_x' }));
    fixture.detectChanges();

    const panel = q('[data-testid="key-secret-once"]');
    expect(panel?.getAttribute('role')).toBe('status');
    expect(panel?.getAttribute('aria-live')).toBe('polite');
    // The announced text must convey "shown once / copy now", not just contain the secret.
    expect(panel?.textContent).toContain('shown once');
    expect(panel?.textContent).toContain("you won't be able to see it again");
  });

  it('restores focus to the Create key button (not body) when the secret panel is dismissed', () => {
    load(makeBoard(), [], []);
    document.body.appendChild(fixture.nativeElement);
    cmp.createKey();
    mock
      .expectOne(`/api/v1/businesses/${biz}/feedback/boards/${boardId}/keys`)
      .flush(makeKey({ id: 'k2', has_secret: true, secret: 'fbs_x' }));
    fixture.detectChanges();

    // dismissSecret() is what both the mouse-click and keyboard (Enter/Space) activation of the
    // native <button data-testid="key-secret-dismiss"> invoke, so exercising it directly covers
    // both triggers.
    cmp.dismissSecret();
    fixture.detectChanges();

    const createBtn = q('[data-testid="key-create"]');
    expect(document.activeElement).toBe(createBtn);
    expect(document.activeElement).not.toBe(document.body);
    fixture.nativeElement.remove();
  });

  it('shows a "secret set" indicator only for keys with has_secret true', () => {
    load(
      makeBoard(),
      [],
      [
        makeKey({ id: 'k1', has_secret: true }),
        makeKey({ id: 'k2', publishable_key: 'fbk_def', has_secret: false }),
      ],
    );
    const rows = fixture.nativeElement.querySelectorAll('[data-testid="key-row"]');
    expect(rows[0].querySelector('[data-testid="key-has-secret-cell"]')?.textContent).toContain(
      'Secret set',
    );
    expect(rows[1].querySelector('[data-testid="key-has-secret-cell"]')?.textContent?.trim()).toBe(
      '',
    );
  });

  it('shows a Verified badge only for posts with identity_verified true', () => {
    load(makeBoard(), [
      makePost({ id: 'p1', identity_verified: true }),
      makePost({ id: 'p2', title: 'Other', identity_verified: false }),
    ]);
    const rows = fixture.nativeElement.querySelectorAll('[data-testid="post-row"]');
    expect(rows[0].querySelector('[data-testid="post-verified-badge"]')).toBeTruthy();
    expect(rows[0].textContent).toContain('Verified');
    expect(rows[1].querySelector('[data-testid="post-verified-badge"]')).toBeFalsy();
    expect(rows[1].textContent).not.toContain('Verified');
  });
});
