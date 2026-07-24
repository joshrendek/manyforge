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
});
