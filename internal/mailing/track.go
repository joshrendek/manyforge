package mailing

import (
	"context"
	"io"
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	mailtoken "github.com/manyforge/manyforge/internal/mailing/token"
)

// Confirm activates an unexpired one-time token. Malformed and unknown tokens are deliberately
// successful no-ops so the root route cannot become a token-validity oracle.
func (s *Service) Confirm(ctx context.Context, raw string) error {
	if s.Tokens == nil {
		return nil
	}
	hash, err := mailtoken.HashConfirmation(raw)
	if err != nil {
		return nil
	}
	return s.DB.WithTx(ctx, func(tx pgx.Tx) error {
		var rows int
		return tx.QueryRow(ctx, `SELECT mailing_confirm($1)`, hash).Scan(&rows)
	})
}

// Unsubscribe verifies the stateless token before database access. Invalid and valid-unknown
// tokens are deliberately successful no-ops and therefore receive byte-identical responses.
func (s *Service) Unsubscribe(ctx context.Context, raw string) error {
	if s.Tokens == nil {
		return nil
	}
	subscriberID, campaignID, err := s.Tokens.DecodeUnsubscribe(raw)
	if err != nil {
		return nil
	}
	return s.DB.WithTx(ctx, func(tx pgx.Tx) error {
		var rows int
		if err := tx.QueryRow(ctx, `SELECT mailing_unsubscribe($1,$2,$3)`, subscriberID, campaignID, "link").Scan(&rows); err != nil {
			return err
		}
		var recorded bool
		return tx.QueryRow(ctx, `SELECT mailing_record_unsubscribe($1,$2)`, subscriberID, campaignID).Scan(&recorded)
	})
}

func (s *Service) recordTrack(ctx context.Context, deliveryID uuid.UUID, kind, target, ip, ua string) error {
	return s.DB.WithTx(ctx, func(tx pgx.Tx) error {
		var recorded bool
		var ipArg any
		if ip != "" {
			ipArg = ip
		}
		return tx.QueryRow(ctx, `SELECT mailing_record_track($1,$2,$3,$4,$5)`,
			deliveryID, kind, nullIfEmpty(target), ipArg, nullIfEmpty(ua)).Scan(&recorded)
	})
}

// RootRoutes mounts the stable, principal-less confirmation and unsubscribe surface. The
// limiter lives here because these routes are intentionally outside the shared /api/v1 group.
func (h *PublicHandler) RootRoutes(r chi.Router) {
	mount := func(root chi.Router) {
		root.Get("/m/confirm/{token}", h.confirmPage)
		root.Post("/m/confirm/{token}", h.confirm)
		root.Get("/m/u/{token}", h.unsubscribePage)
		root.Post("/m/u/{token}", h.unsubscribe)
		root.Get("/m/o/{token}", h.trackOpen)
		root.Get("/m/c/{token}", h.trackClick)
	}
	if h.IngestLimit == nil {
		mount(r)
		return
	}
	r.Group(func(root chi.Router) {
		root.Use(h.IngestLimit)
		mount(root)
	})
}

// A constant 1x1 transparent GIF is returned for every open token outcome, including malformed
// and unknown tokens, so image fetches cannot probe delivery existence.
var transparentGIF = []byte{
	0x47, 0x49, 0x46, 0x38, 0x39, 0x61, 0x01, 0x00, 0x01, 0x00, 0x80, 0x00,
	0x00, 0x00, 0x00, 0x00, 0xff, 0xff, 0xff, 0x21, 0xf9, 0x04, 0x01, 0x00, 0x00,
	0x00, 0x00, 0x2c, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, 0x02,
	0x02, 0x44, 0x01, 0x00, 0x3b,
}

func (h *PublicHandler) trackOpen(w http.ResponseWriter, r *http.Request) {
	if h.Service.Tokens != nil {
		if deliveryID, err := h.Service.Tokens.DecodeOpen(chi.URLParam(r, "token")); err == nil {
			ip := ""
			if h.ClientIP != nil {
				ip = h.ClientIP(r)
			}
			if err := h.Service.recordTrack(r.Context(), deliveryID, "open", "", ip, r.UserAgent()); err != nil {
				h.logger().ErrorContext(r.Context(), "mailing open tracking failed", "err", err)
			}
		}
	}
	w.Header().Set("Content-Type", "image/gif")
	w.Header().Set("Cache-Control", "no-store, private")
	w.Header().Set("Content-Length", "43")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(transparentGIF)
}

func (h *PublicHandler) trackClick(w http.ResponseWriter, r *http.Request) {
	if h.Service.Tokens == nil {
		http.NotFound(w, r)
		return
	}
	deliveryID, target, err := h.Service.Tokens.DecodeClick(chi.URLParam(r, "token"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	parsed, err := url.Parse(target)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		http.NotFound(w, r)
		return
	}
	ip := ""
	if h.ClientIP != nil {
		ip = h.ClientIP(r)
	}
	if err := h.Service.recordTrack(r.Context(), deliveryID, "click", target, ip, r.UserAgent()); err != nil {
		h.logger().ErrorContext(r.Context(), "mailing click tracking failed", "err", err)
	}
	http.Redirect(w, r, target, http.StatusFound)
}

const confirmPageHTML = "<!doctype html><html><body><main><h1>Confirm subscription</h1><form method=post><button type=submit>Confirm</button></form></main></body></html>"
const unsubscribePageHTML = "<!doctype html><html><body><main><h1>Unsubscribe</h1><form method=post><button type=submit>Unsubscribe</button></form></main></body></html>"
const donePageHTML = "<!doctype html><html><body><main><h1>All set</h1><p>Your request has been processed.</p></main></body></html>"

func writeMailingPage(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, body)
}

func (h *PublicHandler) confirmPage(w http.ResponseWriter, _ *http.Request) {
	writeMailingPage(w, confirmPageHTML)
}

func (h *PublicHandler) confirm(w http.ResponseWriter, r *http.Request) {
	if err := h.Service.Confirm(r.Context(), chi.URLParam(r, "token")); err != nil {
		h.logger().ErrorContext(r.Context(), "mailing confirm failed", "err", err)
	}
	writeMailingPage(w, donePageHTML)
}

func (h *PublicHandler) unsubscribePage(w http.ResponseWriter, _ *http.Request) {
	writeMailingPage(w, unsubscribePageHTML)
}

func (h *PublicHandler) unsubscribe(w http.ResponseWriter, r *http.Request) {
	if err := h.Service.Unsubscribe(r.Context(), chi.URLParam(r, "token")); err != nil {
		h.logger().ErrorContext(r.Context(), "mailing unsubscribe failed", "err", err)
	}
	// RFC 8058 one-click callers require a success response with no token-dependent body.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
}
