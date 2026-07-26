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
	"golang.org/x/text/language"

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

const cloudflareCountryHeader = "CF-IPCountry"
const cloudflareConnectingIPHeader = "CF-Connecting-IP"

// validAlpha2Country is a precomputed lookup table of CLDR regions that are real, non-private
// countries.
// The hot collect path can therefore validate an alpha-2 value without reparsing it per request.
var validAlpha2Country = func() [26][26]bool {
	var valid [26][26]bool
	for a := byte('A'); a <= 'Z'; a++ {
		for b := byte('A'); b <= 'Z'; b++ {
			region, err := language.ParseRegion(string([]byte{a, b}))
			valid[a-'A'][b-'A'] = err == nil && region.IsCountry() && !region.IsPrivateUse()
		}
	}
	return valid
}()

// PublicHandler serves the principal-less analytics surface: the snippet and the collect endpoint.
// TrustCloudflareCountryHeader must only be enabled behind an origin-isolated trusted edge.
type PublicHandler struct {
	DB      *appdb.DB
	Logger  *slog.Logger
	Metrics *observability.Metrics
	// PerIP bounds collect volume from a single source. Keyed by IP rather than by the
	// caller-supplied key, because the key is attacker-choosable.
	PerIP          ratelimit.Limiter
	TrustedProxies []*net.IPNet
	// CloudflareSourceRanges contains Cloudflare's origin-facing networks. In trusted mode, the
	// forwarded connection source must belong to this set before CF-IPCountry is accepted.
	CloudflareSourceRanges []*net.IPNet
	// TrustCloudflareCountryHeader declares that every request reaches this handler through a
	// trusted edge which overwrites CF-IPCountry. It is also gated per request on the direct peer
	// belonging to TrustedProxies; it must remain false when the origin can be reached through an
	// untrusted path, otherwise a caller could forge its own country.
	TrustCloudflareCountryHeader bool
}

// ResolveClient returns the visitor IP used transiently for hashing/rate limiting and whether this
// request may assert a Cloudflare country header. It first resolves the ingress-forwarded
// connection source and requires both a trusted direct peer and a Cloudflare source range. Only
// then may CF-Connecting-IP replace that edge address with the visitor address; country-header
// trust additionally requires the deployment opt-in.
func (h *PublicHandler) ResolveClient(r *http.Request) (string, bool) {
	sourceIP, trustedPeer := ratelimit.ClientIPAndTrustedPeer(r, h.TrustedProxies)
	cloudflareSource := trustedPeer &&
		ipInNetworks(net.ParseIP(sourceIP), h.CloudflareSourceRanges)
	clientIP := sourceIP
	if cloudflareSource {
		values := r.Header.Values(cloudflareConnectingIPHeader)
		if len(values) == 1 {
			if visitorIP := net.ParseIP(strings.TrimSpace(values[0])); visitorIP != nil {
				clientIP = visitorIP.String()
			}
		}
	}
	return clientIP, h.TrustCloudflareCountryHeader && cloudflareSource
}

func ipInNetworks(ip net.IP, networks []*net.IPNet) bool {
	if ip == nil {
		return false
	}
	for _, network := range networks {
		if network.Contains(ip) {
			return true
		}
	}
	return false
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
	// Query carries ONLY the utm_* parameters the snippet extracted. It is not the page's real
	// query string: sending that would ship session tokens and email addresses to the server, and
	// the server re-filters anyway (ParseUTM is an allowlist).
	Query string `json:"q"`
	// Name is a custom event name. Empty (the usual case) means an automatic pageview.
	Name string `json:"n"`
	// Data holds custom event properties. Bounded and flattened server-side.
	Data map[string]any `json:"d"`
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

	ip, trustCountryHeader := h.ResolveClient(r)
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

	// Every request-derived dimension is normalized or bounded HERE before it touches SQL. The raw
	// IP and UA continue past this point only as hash inputs.
	utm := ParseUTM(req.Query)

	// An unusable custom-event name is DROPPED, not coerced to a pageview: counting someone's
	// malformed event as a pageview would inflate the headline number with traffic that never
	// happened. An empty name is the normal automatic-pageview case and is not an error.
	name := NormalizeEventName(req.Name)
	if req.Name != "" && name == "" {
		h.Metrics.Inc(observability.MetricAnalyticsCollectRejected)
		return
	}

	ev := collectEvent{
		key:      req.Key,
		path:     normalizePath(req.Path),
		referrer: referrerHost(req.Referrer),
		ip:       ip,
		ua:       ua,
		isBot:    IsBot(ua),
		utm:      utm,
		device:   DeviceType(ua),
		browser:  Browser(ua),
		country: cloudflareCountry(
			trustCountryHeader,
			r.Header.Values(cloudflareCountryHeader),
		),
		name:  name,
		props: SanitizeProps(req.Data),
	}
	n, err := h.store(r.Context(), ev)
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

// collectEvent is one fully-derived analytics event. Grouping the arguments keeps the SQL call
// from becoming a positional puzzle at the call site.
type collectEvent struct {
	key      string
	path     string
	referrer string
	ip       string // hash input only; never stored
	ua       string // hash input only; never stored
	isBot    bool
	utm      UTM
	device   string
	browser  string
	country  string            // uppercase ISO 3166-1 alpha-2; "" ⇒ no accepted country value
	name     string            // "" ⇒ automatic pageview
	props    map[string]string // nil ⇒ no custom properties
}

func (h *PublicHandler) store(ctx context.Context, e collectEvent) (int, error) {
	// nil props are sent as an empty object so the column is never NULL and readers do not have to
	// distinguish "no properties" from "not set".
	props := []byte("{}")
	if len(e.props) > 0 {
		if b, err := json.Marshal(e.props); err == nil {
			props = b
		}
	}
	var n int
	err := h.DB.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			"SELECT analytics_collect($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)",
			e.key, e.path, e.referrer, e.ip, e.ua, e.isBot,
			e.utm.Source, e.utm.Medium, e.utm.Campaign,
			e.device, e.browser, e.country,
			e.name, props).Scan(&n)
	})
	return n, err
}

// cloudflareCountry reduces Cloudflare's CF-IPCountry request header to the only location detail
// analytics stores: an ISO 3166-1 alpha-2 country code. PublicHandler.ResolveClient verifies the
// deployment opt-in and request source before setting trusted. Cloudflare uses XX for unknown
// locations and T1 for Tor; neither is a country and both are discarded.
func cloudflareCountry(trusted bool, values []string) string {
	if !trusted || len(values) != 1 {
		return ""
	}
	raw := values[0]
	raw = strings.TrimSpace(raw)
	if len(raw) != 2 {
		return ""
	}
	a, b := raw[0], raw[1]
	if a >= 'a' && a <= 'z' {
		a -= 'a' - 'A'
	}
	if b >= 'a' && b <= 'z' {
		b -= 'a' - 'A'
	}
	// Spell out both Cloudflare sentinels even though the ASCII-letter validation below also
	// rejects T1. Keeping the edge contract visible prevents a future validator relaxation from
	// silently turning Tor into a country bucket. Contract:
	// https://developers.cloudflare.com/network/ip-geolocation/
	if (a == 'X' && b == 'X') || (a == 'T' && b == '1') {
		return ""
	}
	if a < 'A' || a > 'Z' || b < 'A' || b > 'Z' {
		return ""
	}
	if !validAlpha2Country[a-'A'][b-'A'] {
		return ""
	}
	return string([]byte{a, b})
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
