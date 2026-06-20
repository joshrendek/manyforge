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
	defer resp.Body.Close()

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

// PostReview posts a review comment to the given pull request.
// A 404 maps to errs.ErrNotFound; other non-2xx become generic errors.
func (c *client) PostReview(ctx context.Context, prNumber int, r connectors.Review) (connectors.ReviewRef, error) {
	url := fmt.Sprintf("%s/repos/%s/pulls/%d/reviews", c.apiBase, c.repo, prNumber)
	payload, err := json.Marshal(map[string]any{"event": "COMMENT", "body": r.Body})
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
	defer resp.Body.Close()

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
