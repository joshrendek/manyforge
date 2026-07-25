package analytics

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	appdb "github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/observability"
	"github.com/manyforge/manyforge/internal/platform/ratelimit"
)

// maxCollectBytes caps a collect body. A pageview beacon is a few hundred bytes; anything larger
// is abuse. Set here rather than relying on global middleware — each ingress helper bounds itself.
const maxCollectBytes int64 = 4 << 10

// maxPathLen bounds a stored path so a hostile site cannot write unbounded strings into the
// rollup's GROUP BY key.
const maxPathLen = 512

// PublicHandler serves the principal-less analytics surface: the snippet and the collect endpoint.
type PublicHandler struct {
	DB      *appdb.DB
	Logger  *slog.Logger
	Metrics *observability.Metrics
	// PerIP bounds collect volume from a single source. Keyed by IP rather than by the
	// caller-supplied key, because the key is attacker-choosable and TokenBucket never evicts.
	PerIP          ratelimit.Limiter
	TrustedProxies []*net.IPNet
}

// CollectRoutes mounts the public collect endpoint.
//
// OPTIONS is handled alongside POST because the XHR fallback (used when sendBeacon is missing)
// sets Content-Type: application/json, which is not a CORS "simple" content type and therefore
// triggers a preflight. Without an OPTIONS route those browsers collect nothing.
func (h *PublicHandler) CollectRoutes(r chi.Router) {
	r.Post("/a/e", h.collect)
	r.Options("/a/e", h.preflight)
}

// corsHeaders opens the collect endpoint to every origin.
//
// This is correct rather than lax: the endpoint exists to be called from arbitrary tenant sites,
// it takes no cookies and no Authorization header, and it returns no data. Credentials are NOT
// allowed (and `*` could not be combined with them anyway), so a wildcard here grants an attacker
// nothing they could not already do with curl.
func corsHeaders(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	// Cache the preflight so a browser that does send one is not doing it per pageview.
	w.Header().Set("Access-Control-Max-Age", "86400")
	w.Header().Set("Vary", "Origin")
}

func (h *PublicHandler) preflight(w http.ResponseWriter, _ *http.Request) {
	corsHeaders(w)
	w.WriteHeader(http.StatusNoContent)
}

type collectRequest struct {
	Key      string `json:"k"`
	Path     string `json:"p"`
	Referrer string `json:"r"`
}

// collect stores one pageview.
//
// It ALWAYS answers 204, whatever happens: unknown key, revoked key, malformed body, bot. A
// public collect endpoint must not confirm which keys exist, and a sendBeacon has no error
// handling to receive a status anyway — so a varying status would leak information to an attacker
// while telling a legitimate site nothing.
func (h *PublicHandler) collect(w http.ResponseWriter, r *http.Request) {
	corsHeaders(w)
	defer w.WriteHeader(http.StatusNoContent)

	ip := ratelimit.ClientIP(r, h.TrustedProxies)
	if h.PerIP != nil && !h.PerIP.Allow(ip) {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxCollectBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return
	}
	var req collectRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return
	}
	if req.Key == "" {
		return
	}

	ua := r.Header.Get("User-Agent")
	// The IP and UA are passed straight through to the SECURITY DEFINER function, which hashes
	// them into the visitor hash inside the same statement that inserts. They are never persisted
	// and must never be logged.
	n, err := h.store(r.Context(), req.Key, normalizePath(req.Path), referrerHost(req.Referrer), ip, ua, IsBot(ua))
	if err != nil {
		// No key, path, IP, or UA in this log line — only that a write failed.
		h.Logger.ErrorContext(r.Context(), "analytics collect", "err", err)
		h.Metrics.Inc(observability.MetricAnalyticsCollectFailed)
		return
	}
	if n > 0 {
		h.Metrics.Inc(observability.MetricAnalyticsCollected)
	} else {
		h.Metrics.Inc(observability.MetricAnalyticsCollectRejected)
	}
}

func (h *PublicHandler) store(ctx context.Context, key, path, ref, ip, ua string, isBot bool) (int, error) {
	var n int
	err := h.DB.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			"SELECT analytics_collect($1,$2,$3,$4,$5,$6)",
			key, path, ref, ip, ua, isBot).Scan(&n)
	})
	return n, err
}

// normalizePath strips the query string and fragment, enforces a leading slash, drops a trailing
// slash (so "/docs" and "/docs/" are one page), and bounds the length.
//
// The query string is dropped rather than kept because it routinely carries session tokens, email
// addresses, and UTM parameters — storing it would quietly turn a pageview table into a PII store.
func normalizePath(p string) string {
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	for len(p) > 1 && strings.HasSuffix(p, "/") {
		p = strings.TrimSuffix(p, "/")
	}
	if len(p) > maxPathLen {
		p = p[:maxPathLen]
	}
	return p
}

// referrerHost reduces a referrer to its host. The snippet already sends only a host, but this is
// defense in depth: the endpoint is public and a caller can send whatever it likes, so a full URL
// must never reach the database.
func referrerHost(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	if strings.Contains(ref, "/") || strings.Contains(ref, ":") {
		if u, err := url.Parse(ref); err == nil && u.Hostname() != "" {
			ref = u.Hostname()
		} else {
			return ""
		}
	}
	ref = strings.TrimPrefix(strings.ToLower(ref), "www.")
	// A host has no spaces and is bounded; anything else is junk or an injection attempt.
	if ref == "" || len(ref) > 253 || strings.ContainsAny(ref, " \t\n\r/?#@") {
		return ""
	}
	return ref
}

// botUAs are substrings matched case-insensitively against the User-Agent. This is a pragmatic
// denylist, not a complete one — it removes the obvious crawler noise that would otherwise
// dominate a small site's numbers. Bot hits are still STORED (with is_bot true) so the filter can
// be audited and revised; they are excluded at rollup time.
var botUAs = []string{
	"bot", "crawler", "spider", "slurp", "curl", "wget", "python-requests", "go-http-client",
	"headlesschrome", "phantomjs", "puppeteer", "playwright", "lighthouse", "pingdom",
	"uptimerobot", "monitoring", "preview", "facebookexternalhit", "embedly", "quora link",
	"scrapy", "ahrefs", "semrush", "mj12", "dotbot", "petalbot", "bytespider",
}

// IsBot reports whether a User-Agent looks automated.
func IsBot(ua string) bool {
	if ua == "" {
		// A browser always sends a UA. An empty one is a script or a stripped proxy.
		return true
	}
	l := strings.ToLower(ua)
	for _, b := range botUAs {
		if strings.Contains(l, b) {
			return true
		}
	}
	return false
}
