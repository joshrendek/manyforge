package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"
)

// codexModelsClientVersion is the client_version sent to OpenAI's per-plan Codex catalog. The
// endpoint only returns models whose minimal_client_version <= this value, so it must track the
// codex CLI: too low hides new models; too high can surface models the sandbox's opencode cannot
// yet run. Pinned to the current codex CLI release (github.com/openai/codex). Bump on new releases
// — or derive it dynamically from the latest release tag (follow-up).
const codexModelsClientVersion = "0.144.6"

// codexBackendBase is the ChatGPT Codex backend the codex CLI reads its model catalog from. The
// per-account, per-plan list lives under /models (auth is the same OAuth token used for
// completions plus the ChatGPT-Account-Id header).
const codexBackendBase = "https://chatgpt.com/backend-api/codex"

// CodexBackendModels fetches a connected account's live Codex model list from chatgpt.com. The
// list is per-plan and client_version gated (not per-business), so results are cached per account
// id. HTTP MUST be an SSRF-safe client (netsafe.NewClient).
type CodexBackendModels struct {
	HTTP *http.Client
	Base string        // default codexBackendBase
	TTL  time.Duration // default 1h

	mu    sync.Mutex
	cache map[string]codexModelsEntry
	now   func() time.Time
}

type codexModelsEntry struct {
	models  []string
	expires time.Time
}

func (c *CodexBackendModels) base() string {
	if c.Base != "" {
		return c.Base
	}
	return codexBackendBase
}

func (c *CodexBackendModels) ttl() time.Duration {
	if c.TTL > 0 {
		return c.TTL
	}
	return time.Hour
}

func (c *CodexBackendModels) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// ListModels returns the account's user-visible Codex model slugs, flagship-first (priority order).
// It filters to visibility=="list" — the same set the codex CLI shows — dropping hidden/internal
// models (older -mini tails, codex-auto-review). Cached per account id for TTL.
func (c *CodexBackendModels) ListModels(ctx context.Context, accessToken, accountID string) ([]string, error) {
	c.mu.Lock()
	if e, ok := c.cache[accountID]; ok && c.clock().Before(e.expires) {
		m := e.models
		c.mu.Unlock()
		return m, nil
	}
	c.mu.Unlock()

	// Fetch OUTSIDE the lock: never hold the mutex across a blocking HTTP call (cf. manyforge-9v9).
	models, err := c.fetch(ctx, accessToken, accountID)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	if c.cache == nil {
		c.cache = make(map[string]codexModelsEntry)
	}
	c.cache[accountID] = codexModelsEntry{models: models, expires: c.clock().Add(c.ttl())}
	c.mu.Unlock()
	return models, nil
}

func (c *CodexBackendModels) fetch(ctx context.Context, accessToken, accountID string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.base()+"/models?client_version="+codexModelsClientVersion, nil)
	if err != nil {
		return nil, fmt.Errorf("codex models request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("ChatGPT-Account-Id", accountID)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "codex_cli_rs/"+codexModelsClientVersion)
	req.Header.Set("originator", "codex_cli_rs")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("codex models request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("codex models status %d", resp.StatusCode)
	}
	var parsed struct {
		Models []struct {
			Slug       string `json:"slug"`
			Visibility string `json:"visibility"`
			Priority   int    `json:"priority"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("codex models decode: %w", err)
	}
	type sm struct {
		slug string
		prio int
	}
	visible := make([]sm, 0, len(parsed.Models))
	for _, m := range parsed.Models {
		if m.Visibility == "list" && m.Slug != "" {
			visible = append(visible, sm{m.Slug, m.Priority})
		}
	}
	sort.SliceStable(visible, func(i, j int) bool { return visible[i].prio < visible[j].prio })
	out := make([]string, 0, len(visible))
	for _, m := range visible {
		out = append(out, m.slug)
	}
	return out, nil
}
