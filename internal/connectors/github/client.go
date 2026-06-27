package github

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/manyforge/manyforge/internal/connectors"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

type client struct {
	http    *http.Client
	apiBase string // e.g. https://api.github.com
	repo    string // owner/name
	token   string
}

func (c *client) authHeader() string {
	return "Bearer " + c.token
}

// CloneURL returns the HTTPS clone URL for the repo. Host-side git authentication
// is injected via the Authorization header (see BasicAuthHeader), not the URL.
func (c *client) CloneURL() string {
	host := "github.com"
	if !strings.Contains(c.apiBase, "api.github.com") {
		host = strings.TrimPrefix(strings.TrimPrefix(c.apiBase, "https://"), "http://")
		host = strings.TrimSuffix(host, "/api/v3")
	}
	return fmt.Sprintf("https://%s/%s.git", host, c.repo)
}

// FetchPR returns metadata for the given pull request number.
// A 404 from GitHub is mapped to errs.ErrNotFound; other non-2xx become generic errors.
func (c *client) FetchPR(ctx context.Context, prNumber int) (connectors.PullRequest, error) {
	url := fmt.Sprintf("%s/repos/%s/pulls/%d", c.apiBase, c.repo, prNumber)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return connectors.PullRequest{}, fmt.Errorf("github: build fetch pr request: %w", err)
	}
	req.Header.Set("Authorization", c.authHeader())
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := c.http.Do(req)
	if err != nil {
		return connectors.PullRequest{}, fmt.Errorf("github: fetch pr: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return connectors.PullRequest{}, fmt.Errorf("github: pr %d: %w", prNumber, errs.ErrNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		return connectors.PullRequest{}, fmt.Errorf("github: fetch pr status %d", resp.StatusCode)
	}

	var body struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		State  string `json:"state"`
		Merged bool   `json:"merged"`
		Head   struct {
			SHA string `json:"sha"`
			Ref string `json:"ref"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return connectors.PullRequest{}, fmt.Errorf("github: decode pr: %w", err)
	}

	state := body.State
	if body.Merged {
		state = "merged"
	}
	return connectors.PullRequest{
		Number:  body.Number,
		Title:   body.Title,
		HeadSHA: body.Head.SHA,
		HeadRef: body.Head.Ref,
		BaseRef: body.Base.Ref,
		State:   state,
	}, nil
}

// changedFilesPageCap bounds pagination of the PR files endpoint (100/page) so a
// pathological PR can't drive unbounded requests; 20 pages = 2000 files.
const changedFilesPageCap = 20

// ChangedLines fetches the PR's changed files and returns, per new-version path,
// the set of new-side line numbers that are part of the diff (valid inline-comment
// targets). A 404 maps to errs.ErrNotFound; other non-2xx become generic errors.
func (c *client) ChangedLines(ctx context.Context, prNumber int) (map[string]map[int]bool, error) {
	out := map[string]map[int]bool{}
	for page := 1; page <= changedFilesPageCap; page++ {
		url := fmt.Sprintf("%s/repos/%s/pulls/%d/files?per_page=100&page=%d", c.apiBase, c.repo, prNumber, page)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, fmt.Errorf("github: build pr files request: %w", err)
		}
		req.Header.Set("Authorization", c.authHeader())
		req.Header.Set("Accept", "application/vnd.github+json")

		resp, err := c.http.Do(req)
		if err != nil {
			return nil, fmt.Errorf("github: fetch pr files: %w", err)
		}
		if resp.StatusCode == http.StatusNotFound {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("github: pr %d: %w", prNumber, errs.ErrNotFound)
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("github: fetch pr files status %d", resp.StatusCode)
		}
		var files []struct {
			Filename string `json:"filename"`
			Patch    string `json:"patch"`
		}
		derr := json.NewDecoder(resp.Body).Decode(&files)
		_ = resp.Body.Close()
		if derr != nil {
			return nil, fmt.Errorf("github: decode pr files: %w", derr)
		}
		for _, f := range files {
			out[f.Filename] = commentableLines(f.Patch)
		}
		if len(files) < 100 {
			break
		}
	}
	return out, nil
}

// reviewMarker is a hidden HTML comment embedded in a review body so a retry can
// recognise an already-posted review and avoid duplicating it. GitHub renders HTML
// comments invisibly.
func reviewMarker(dedupKey string) string {
	return "<!-- manyforge-review-id: " + dedupKey + " -->"
}

// findReviewByMarker returns an existing review on the PR whose body carries the
// marker, if any. Best-effort: a fetch/parse error returns found=false (caller
// then posts) rather than blocking the review.
func (c *client) findReviewByMarker(ctx context.Context, prNumber int, marker string) (connectors.ReviewRef, bool) {
	const pageCap = 10 // 100/page → up to 1000 reviews scanned
	for page := 1; page <= pageCap; page++ {
		url := fmt.Sprintf("%s/repos/%s/pulls/%d/reviews?per_page=100&page=%d", c.apiBase, c.repo, prNumber, page)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return connectors.ReviewRef{}, false
		}
		req.Header.Set("Authorization", c.authHeader())
		req.Header.Set("Accept", "application/vnd.github+json")
		resp, err := c.http.Do(req)
		if err != nil {
			return connectors.ReviewRef{}, false
		}
		var reviews []struct {
			ID      int64  `json:"id"`
			Body    string `json:"body"`
			HTMLURL string `json:"html_url"`
		}
		derr := json.NewDecoder(resp.Body).Decode(&reviews)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || derr != nil {
			return connectors.ReviewRef{}, false
		}
		for _, rv := range reviews {
			if strings.Contains(rv.Body, marker) {
				return connectors.ReviewRef{ExternalID: fmt.Sprintf("%d", rv.ID), URL: rv.HTMLURL}, true
			}
		}
		if len(reviews) < 100 {
			break
		}
	}
	return connectors.ReviewRef{}, false
}

// PostReview posts a review to the given pull request: a summary body plus any
// inline diff comments. When r.DedupKey is set it is idempotent — a hidden marker
// is embedded in the body and any prior review carrying the same marker is reused
// instead of posting a duplicate (a worker retry re-runs the whole job).
// A 404 maps to errs.ErrNotFound; other non-2xx become generic errors.
func (c *client) PostReview(ctx context.Context, prNumber int, r connectors.Review) (connectors.ReviewRef, error) {
	postBody := r.Body
	if r.DedupKey != "" {
		marker := reviewMarker(r.DedupKey)
		if ref, found := c.findReviewByMarker(ctx, prNumber, marker); found {
			return ref, nil // already posted on a prior attempt — don't duplicate
		}
		postBody = postBody + "\n\n" + marker
	}
	url := fmt.Sprintf("%s/repos/%s/pulls/%d/reviews", c.apiBase, c.repo, prNumber)
	reviewBody := map[string]any{"event": "COMMENT", "body": postBody}
	if r.CommitID != "" {
		reviewBody["commit_id"] = r.CommitID
	}
	if len(r.Comments) > 0 {
		comments := make([]map[string]any, 0, len(r.Comments))
		for _, cm := range r.Comments {
			comments = append(comments, map[string]any{
				"path": cm.Path,
				"line": cm.Line,
				"side": "RIGHT",
				"body": cm.Body,
			})
		}
		reviewBody["comments"] = comments
	}
	payload, err := json.Marshal(reviewBody)
	if err != nil {
		return connectors.ReviewRef{}, fmt.Errorf("github: marshal review: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return connectors.ReviewRef{}, fmt.Errorf("github: build post review request: %w", err)
	}
	req.Header.Set("Authorization", c.authHeader())
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return connectors.ReviewRef{}, fmt.Errorf("github: post review: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return connectors.ReviewRef{}, fmt.Errorf("github: pr %d: %w", prNumber, errs.ErrNotFound)
	}
	if resp.StatusCode/100 != 2 {
		return connectors.ReviewRef{}, fmt.Errorf("github: post review status %d", resp.StatusCode)
	}

	var body struct {
		ID      int64  `json:"id"`
		HTMLURL string `json:"html_url"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return connectors.ReviewRef{
		ExternalID: fmt.Sprintf("%d", body.ID),
		URL:        body.HTMLURL,
	}, nil
}

// BasicAuthHeader builds the Authorization header value used for host-side git clone auth.
// The format is the git credential helper extraHeader format used with http.extraHeader.
func BasicAuthHeader(token string) string {
	return "AUTHORIZATION: basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
}
