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
	// PostReview posts a single review (summary + findings) to the pull request. Advisory only.
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

type Review struct {
	Summary  string
	Findings []Finding
	Body     string // rendered markdown body actually posted
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

type CreateRepoConnectorInput struct {
	Type                string
	DisplayName         string
	BaseURL             string
	Repo                string
	AllowPrivateBaseURL bool
	APIToken            string
}
