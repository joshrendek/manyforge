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

// assumeLiveProviders cannot be cheaply probed, so they are assumed live and covered
// reactively by the worker retry + the dimension's fallback_chain:
//
//   - anthropic: no cheap unauthenticated probe endpoint.
//   - huggingface: a ZeroGPU Space sleeps after inactivity and takes 30-60s to cold-start,
//     which no probe budget can accommodate (defaultProbeTimeout is 3s, and raising it would
//     stall every review behind a sleeping Space). Probing would therefore skip the lane on a
//     perfectly healthy Space every time it had gone idle. See manyforge-bhx.
var assumeLiveProviders = map[string]bool{"anthropic": true, "huggingface": true}

// Live probes OpenAI-compatible providers with GET {base_url}/models through a netsafe
// client that honors the credential's private-host opt-in — so a LAN address like
// 192.168.x.x is reachable only when allow_private_base_url is set, matching every other
// outbound path (no SSRF regression). Any transport error (connection refused, DNS failure,
// blocked private IP, timeout) or non-2xx status ⇒ not live.
func (p httpProber) Live(ctx context.Context, cred AICredential) bool {
	if assumeLiveProviders[strings.ToLower(cred.Provider)] {
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
