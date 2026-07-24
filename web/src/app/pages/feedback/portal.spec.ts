import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { ActivatedRoute, provideRouter } from '@angular/router';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { PublicPost } from '../../core/public-feedback.service';
import { FeedbackPortalComponent } from './portal';

const key = 'fbk_demo';

function makePost(over: Partial<PublicPost> = {}): PublicPost {
  return {
    id: 'p1',
    title: 'Dark mode',
    body: 'Please',
    status: 'open',
    vote_count: 4,
    created_at: '',
    ...over,
  };
}

describe('FeedbackPortalComponent', () => {
  let fixture: ComponentFixture<FeedbackPortalComponent>;
  let cmp: FeedbackPortalComponent;
  let mock: HttpTestingController;

  function load(posts: PublicPost[] = [makePost()]): void {
    fixture = TestBed.createComponent(FeedbackPortalComponent);
    cmp = fixture.componentInstance;
    fixture.detectChanges(); // ngOnInit → listPosts
    mock.expectOne(`/api/v1/feedback/public/${key}/posts`).flush({ items: posts });
    fixture.detectChanges();
  }

  function q(sel: string): HTMLElement | null {
    return fixture.nativeElement.querySelector(sel) as HTMLElement | null;
  }

  beforeEach(() => {
    localStorage.clear();
    TestBed.configureTestingModule({
      providers: [
        provideHttpClient(),
        provideHttpClientTesting(),
        provideRouter([]),
        { provide: ActivatedRoute, useValue: { snapshot: { paramMap: new Map([['key', key]]) } } },
      ],
    });
    mock = TestBed.inject(HttpTestingController);
    document.documentElement.setAttribute('data-theme', 'light');
  });
  afterEach(() => {
    localStorage.clear();
    document.documentElement.setAttribute('data-theme', 'light');
  });

  it('renders public posts with an upvote button (no auth)', () => {
    load();
    expect(q('[data-testid="portal-post"]')).toBeTruthy();
    expect(q('[data-testid="portal-post-title"]')?.textContent).toContain('Dark mode');
    expect(q('[data-testid="portal-vote-count"]')?.textContent).toContain('4');
  });

  it('shows the unavailable state on a 401 (unknown/revoked key or private board)', () => {
    fixture = TestBed.createComponent(FeedbackPortalComponent);
    cmp = fixture.componentInstance;
    fixture.detectChanges();
    mock
      .expectOne(`/api/v1/feedback/public/${key}/posts`)
      .flush(
        { code: 'UNAUTHORIZED', message: 'unauthorized' },
        { status: 401, statusText: 'Unauthorized' },
      );
    fixture.detectChanges();
    expect(q('[data-testid="portal-unavailable"]')).toBeTruthy();
    expect(cmp.unavailable()).toBe(true);
  });

  it('submits a new idea with an anonymous author identity then reloads', () => {
    load();
    cmp.title = 'Add SSO';
    cmp.submit();
    const req = mock.expectOne(`/api/v1/feedback/public/${key}/posts`);
    expect(req.request.method).toBe('POST');
    expect(req.request.body.title).toBe('Add SSO');
    expect(typeof req.request.body.author_identity).toBe('string');
    expect(req.request.body.author_identity.length).toBeGreaterThan(0);
    req.flush({ id: 'p2', title: 'Add SSO', status: 'open', vote_count: 0 });
    mock
      .expectOne(`/api/v1/feedback/public/${key}/posts`)
      .flush({ items: [makePost(), makePost({ id: 'p2', title: 'Add SSO', vote_count: 0 })] });
    fixture.detectChanges();
    expect(cmp.title).toBe('');
  });

  it('upvotes a post with the device identity and reflects the new count', () => {
    load();
    cmp.upvote(makePost());
    const req = mock.expectOne(`/api/v1/feedback/public/${key}/posts/p1/votes`);
    expect(req.request.method).toBe('POST');
    expect(typeof req.request.body.voter_identity).toBe('string');
    req.flush({ voted: true, vote_count: 5 });
    fixture.detectChanges();
    expect(cmp.posts()[0].vote_count).toBe(5);
    expect(cmp.voted().has('p1')).toBe(true);
  });
});
