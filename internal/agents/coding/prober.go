package coding

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/manyforge/manyforge/internal/platform/netsafe"
)

// reviewbotProber decides whether a reviewbot's provider endpoint is reachable, so the
// fallback chain can skip a down primary WITHOUT spinning up a doomed review. It never
// surfaces to the PR — a probe result only steers bot selection.
type reviewbotProber interface {
	Live(ctx context.Context, cred AICredential) bool
}

// defaultProbeTimeout bounds a single liveness probe. Short: a down LAN server should be
// detected in a couple seconds, not stall the review.
const defaultProbeTimeout = 3 * time.Second

// httpProber is the production reviewbotProber.
type httpProber struct{ Timeout time.Duration }

// Live probes OpenAI-compatible providers with GET {base_url}/models through a netsafe
// client that honors the credential's private-host opt-in — so a LAN address like
// 192.168.x.x is reachable only when allow_private_base_url is set, matching every other
// outbound path (no SSRF regression). Any transport error (connection refused, DNS
// failure, blocked private IP, timeout) or non-2xx status ⇒ not live. huggingface IS
// probed: the router serves GET /v1/models publicly and fast.
//
// Two providers are assumed live (no HTTP probe): anthropic has no cheap unauthenticated
// probe endpoint; openai_codex's ChatGPT backend answers GET /models only to a request
// carrying ChatGPT-Account-Id + originator + a versioned codex User-Agent + a fresh OAuth
// access token (a bare Bearer GET 403s — manyforge-6fx), so a generic probe here would
// ALWAYS mark a healthy codex down. Codex liveness is instead enforced at credential-resolve
// time: the access token is refreshed-or-fails just before launch, so a genuinely dead codex
// surfaces as an unresolvable candidate the fallback chain already skips. Both are covered
// reactively by the worker retry.
func (p httpProber) Live(ctx context.Context, cred AICredential) bool {
	switch strings.ToLower(cred.Provider) {
	case "anthropic", "openai_codex":
		return true
	}
	if cred.BaseURL == "" {
		return false
	}
	to := p.Timeout
	if to <= 0 {
		to = defaultProbeTimeout
	}
	hc := netsafe.NewClientWithOptions(to, netsafe.Options{
		AllowLoopback: cred.AllowPrivateBaseURL,
		AllowPrivate:  cred.AllowPrivateBaseURL,
	})
	url := strings.TrimRight(cred.BaseURL, "/") + "/models"
	cctx, cancel := context.WithTimeout(ctx, to)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	// A bearer key isn't required by /models on LM Studio/Ollama, but include it when
	// present so a key-gated OpenAI-compat endpoint answers 200 instead of 401.
	if cred.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cred.APIKey)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}
