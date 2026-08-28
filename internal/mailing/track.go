package mailing

import (
	"context"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	mailtoken "github.com/manyforge/manyforge/internal/mailing/token"
)

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
		return tx.QueryRow(ctx, `SELECT mailing_unsubscribe($1,$2,$3)`, subscriberID, campaignID, "link").Scan(&rows)
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
