package githubapp

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

// pullRequestEvent is the subset of the GitHub pull_request webhook payload
// needed to filter (draft/bot-author/fork/non-trigger-action) and resolve
// the installation context before ingesting a review request.
type pullRequestEvent struct {
	Action       string `json:"action"`
	Number       int    `json:"number"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	PullRequest struct {
		Draft bool `json:"draft"`
		User  struct {
			Type string `json:"type"`
		} `json:"user"`
		Head struct {
			SHA  string `json:"sha"`
			Repo *struct {
				ID int64 `json:"id"`
			} `json:"repo"`
		} `json:"head"`
		Base struct {
			Repo struct {
				ID int64 `json:"id"`
			} `json:"repo"`
		} `json:"base"`
	} `json:"pull_request"`
}

// prTriggerAction reports whether a pull_request action means "there is
// new/changed code to review" — the only actions that should enqueue a
// review.
func prTriggerAction(a string) bool {
	switch a {
	case "opened", "synchronize", "reopened", "ready_for_review":
		return true
	}
	return false
}

// handlePullRequestEvent parses a pull_request webhook event, filters out
// draft/bot-authored/fork PRs and non-trigger actions, resolves the
// installation's linked business/agent, and — only if linked and enabled —
// atomically ingests a PR review request. Filtered-out and malformed events
// never reach ResolveInstallation or IngestPRReview, so they never create a
// junk repo_connector (fable m5) or consume a delivery id.
func (h *Handler) handlePullRequestEvent(r *http.Request, body []byte) {
	if h.PRReviews == nil {
		return // webhook wired without the enqueuer (fable m4): no-op, not a crash
	}
	var ev pullRequestEvent
	if err := json.Unmarshal(body, &ev); err != nil || ev.Installation.ID == 0 {
		return
	}
	if !prTriggerAction(ev.Action) {
		return
	}
	// fable m5: reject malformed-but-signed payloads before they can create a
	// junk repo_connector — a genuine GitHub payload always has an "owner/repo"
	// full_name and a non-empty head sha.
	if !strings.Contains(ev.Repository.FullName, "/") || ev.PullRequest.Head.SHA == "" {
		return
	}
	ctx := r.Context()
	// Filters — no DB write, no delivery consumption.
	if ev.PullRequest.Draft {
		h.info(ctx, "pr skipped: draft", "repo", ev.Repository.FullName, "number", ev.Number)
		return
	}
	if ev.PullRequest.User.Type == "Bot" {
		h.info(ctx, "pr skipped: bot author", "repo", ev.Repository.FullName, "number", ev.Number)
		return
	}
	if ev.PullRequest.Head.Repo == nil || ev.PullRequest.Head.Repo.ID != ev.PullRequest.Base.Repo.ID {
		h.info(ctx, "pr skipped: fork", "repo", ev.Repository.FullName, "number", ev.Number)
		return
	}
	ic, ok, err := h.PRReviews.ResolveInstallation(ctx, ev.Installation.ID)
	if err != nil {
		h.log(ctx, "pr: resolve installation context", err)
		return
	}
	if !ok || ic.BusinessID == uuid.Nil || ic.AgentID == uuid.Nil || !ic.Enabled || ic.Suspended || !ic.AgentEnabled {
		h.info(ctx, "pr skipped: installation not linked/enabled",
			"installation_id", ev.Installation.ID, "found", ok)
		return
	}
	_, ingested, err := h.PRReviews.IngestPRReview(ctx, PRReviewInput{
		InstallationID:   ev.Installation.ID,
		DeliveryID:       r.Header.Get("X-GitHub-Delivery"),
		Repo:             ev.Repository.FullName,
		PRNumber:         ev.Number,
		HeadSHA:          ev.PullRequest.Head.SHA,
		BusinessID:       ic.BusinessID,
		TenantRootID:     ic.TenantRootID,
		AgentID:          ic.AgentID,
		AgentPrincipalID: ic.AgentPrincipalID,
	})
	if err != nil {
		h.log(ctx, "pr: ingest review", err)
		return
	}
	if !ingested {
		// Not a caller error — a replayed delivery, an installation over the
		// hourly rate cap, or a duplicate (repo, pr, head_sha) already
		// pending/running/succeeded (fable m3).
		h.info(ctx, "pr skipped: ingest no-op (replay/rate/dup)",
			"repo", ev.Repository.FullName, "number", ev.Number)
	}
}
