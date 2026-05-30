package account

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// Handler exposes the account use cases over HTTP.
type Handler struct{ svc *Service }

// NewHandler builds an account HTTP handler.
func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// PublicRoutes mounts the unauthenticated auth endpoints.
func (h *Handler) PublicRoutes(r chi.Router) {
	r.Post("/auth/signup", h.signup)
	r.Post("/auth/verify-email", h.verifyEmail)
	r.Post("/auth/login", h.login)
	r.Post("/auth/refresh", h.refresh)
	r.Post("/auth/logout", h.logout)
}

// ProtectedRoutes mounts endpoints that require authentication.
func (h *Handler) ProtectedRoutes(r chi.Router) {
	r.Get("/me", h.me)
	r.Patch("/me", h.updateMe)
}

type tokenResp struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
}

func (h *Handler) signup(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email       string `json:"email"`
		DisplayName string `json:"display_name"`
		Password    string `json:"password"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	if _, _, err := h.svc.Signup(r.Context(), in.Email, in.DisplayName, in.Password); err != nil {
		// Duplicate email returns the same 202 as success — no existence oracle (FR-026).
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		httpx.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (h *Handler) verifyEmail(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token string `json:"token"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	if err := h.svc.VerifyEmail(r.Context(), in.Token); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	tp, err := h.svc.Login(r.Context(), in.Email, in.Password)
	if err != nil {
		if errors.Is(err, ErrInvalidCredentials) {
			httpx.WriteJSON(w, http.StatusUnauthorized, httpx.ErrorBody{Code: "INVALID_CREDENTIALS", Message: "invalid credentials"})
			return
		}
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, tokenResp{tp.Access, tp.Refresh, tp.ExpiresIn})
}

func (h *Handler) refresh(w http.ResponseWriter, r *http.Request) {
	var in struct {
		RefreshToken string `json:"refresh_token"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	tp, err := h.svc.Refresh(r.Context(), in.RefreshToken)
	if err != nil {
		httpx.WriteJSON(w, http.StatusUnauthorized, httpx.ErrorBody{Code: "INVALID_CREDENTIALS", Message: "invalid refresh token"})
		return
	}
	httpx.WriteJSON(w, http.StatusOK, tokenResp{tp.Access, tp.Refresh, tp.ExpiresIn})
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	var in struct {
		RefreshToken string `json:"refresh_token"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	if err := h.svc.Logout(r.Context(), in.RefreshToken); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type profileResp struct {
	ID            string `json:"id"`
	Email         string `json:"email"`
	DisplayName   string `json:"display_name"`
	EmailVerified bool   `json:"email_verified"`
	Status        string `json:"status"`
}

func toProfileResp(p Profile) profileResp {
	return profileResp{ID: p.ID.String(), Email: p.Email, DisplayName: p.DisplayName, EmailVerified: p.EmailVerified, Status: p.Status}
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	pid, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteJSON(w, http.StatusUnauthorized, httpx.ErrorBody{Code: "UNAUTHORIZED", Message: "authentication required"})
		return
	}
	p, err := h.svc.GetProfile(r.Context(), pid)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toProfileResp(p))
}

func (h *Handler) updateMe(w http.ResponseWriter, r *http.Request) {
	pid, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteJSON(w, http.StatusUnauthorized, httpx.ErrorBody{Code: "UNAUTHORIZED", Message: "authentication required"})
		return
	}
	var in struct {
		DisplayName string `json:"display_name"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	p, err := h.svc.UpdateProfile(r.Context(), pid, in.DisplayName)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toProfileResp(p))
}
