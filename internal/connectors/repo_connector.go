package connectors

import "context"

// RepoConnector is a code-hosting connector for read-only review.
// SECURITY (spec 007 slice 1): it exposes NO method that can push, commit, or
// open a pull request. The only outbound write is PostReview (advisory comments).
type RepoConnector interface {
	// FetchPR returns metadata for an open pull request (host-side, uses the credential).
	FetchPR(ctx context.Context, prNumber int) (PullRequest, error)
	// CloneURL returns the https clone URL for the repo (host-side clone uses header auth).
	CloneURL() string
	// ChangedLines returns, per changed file path (new-version path), the set of
	// new-side line numbers that are part of the PR diff and therefore valid
	// targets for inline review comments. Files with no commentable lines (binary,
	// pure deletions) still appear with an empty set so callers can scope a review
	// to the changed file list.
	ChangedLines(ctx context.Context, prNumber int) (map[string]map[int]bool, error)
	// PostReview posts a single review (summary + optional inline comments) to the
	// pull request. Advisory only.
	PostReview(ctx context.Context, prNumber int, r Review) (ReviewRef, error)
}

type PullRequest struct {
	Number  int
	Title   string
	HeadSHA string
	BaseRef string
	HeadRef string
	State   string // "open" | "closed" | "merged"
}

type Finding struct {
	File     string `json:"file"`
	Line     *int   `json:"line"`
	Severity string `json:"severity"` // "info" | "warning" | "error"
	Title    string `json:"title"`
	Detail   string `json:"detail"`
}

// ReviewComment is a single inline review comment anchored to a line of the PR
// diff (new/RIGHT side). Line must be a commentable line (see ChangedLines) or
// the host rejects the entire review.
type ReviewComment struct {
	Path string
	Line int
	Body string
}

type Review struct {
	Summary  string
	Findings []Finding
	Body     string          // rendered markdown body actually posted (summary + any non-inline findings)
	CommitID string          // head SHA the inline comments anchor to (optional)
	Comments []ReviewComment // inline diff comments (optional)
	// DedupKey makes posting idempotent. When set, PostReview embeds a hidden
	// marker carrying it in the review body and, before posting, reuses any
	// existing review on the PR that already carries the same marker — so a
	// worker retry of the same review never posts a duplicate.
	DedupKey string
}

type ReviewRef struct {
	ExternalID string // provider review id
	URL        string
}

type ResolvedRepoConnector struct {
	ID                  string
	Type                string
	BaseURL             string
	Repo                string // "owner/name"
	AllowPrivateBaseURL bool
	Config              map[string]any
	Credential          Credential // reuses connectors.Credential (APIToken used as the GitHub token)
}

// CreateRepoConnectorInput is decoded directly from the request body by the
// repo-connector create handler, so its json tags ARE the API contract — they
// must be snake_case to match the OpenAPI spec + the web client (manyforge-elo:
// untagged fields silently rejected snake_case bodies with "display_name required").
type CreateRepoConnectorInput struct {
	Type                string `json:"type"`
	DisplayName         string `json:"display_name"`
	BaseURL             string `json:"base_url"`
	Repo                string `json:"repo"`
	AllowPrivateBaseURL bool   `json:"allow_private_base_url"`
	APIToken            string `json:"api_token"`
}
