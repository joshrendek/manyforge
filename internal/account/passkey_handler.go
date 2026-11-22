package account

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

const passkeyBodyLimit = 64 << 10

// BrowserPasskeyRoutes serves the same-origin browser ceremonies and account
// settings under the SPA's API proxy.
func (h *Handler) BrowserPasskeyRoutes(r chi.Router, limit func(http.Handler) http.Handler) {
	r.Route("/api/passkeys", func(r chi.Router) {
		r.Use(h.passkeyHeaders)
		r.Get("/status", func(w http.ResponseWriter, _ *http.Request) {
			httpx.WriteJSON(w, http.StatusOK, map[string]bool{"enabled": h.svc.Passkeys != nil})
		})
		r.Group(func(pub chi.Router) {
			pub.Use(limit, h.requirePasskeyOrigin)
			pub.Post("/login/begin", h.passkeyLoginBegin)
			pub.Post("/login/finish", h.passkeyLoginFinish)
		})
		r.Group(func(pr chi.Router) {
			pr.Use(httpx.RequireAuth)
			pr.Get("/", h.passkeyList)
			pr.Group(func(write chi.Router) {
				write.Use(limit, h.requirePasskeyOrigin, h.requireRecentPasskeyAuth)
				write.Post("/register/begin", h.passkeyRegisterBegin)
				write.Post("/register/finish", h.passkeyRegisterFinish)
				write.Delete("/{passkeyID}", h.passkeyRemove)
			})
		})
	})
}

func (h *Handler) passkeyHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func (h *Handler) requirePasskeyOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.svc.Passkeys == nil {
			httpx.WriteError(w, r, errs.ErrNotFound)
			return
		}
		origins := r.Header.Values("Origin")
		if len(origins) != 1 || origins[0] != h.svc.Passkeys.origin {
			httpx.WriteJSON(w, http.StatusForbidden, httpx.ErrorBody{Code: "INVALID_ORIGIN", Message: "invalid origin"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *Handler) requireRecentPasskeyAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !h.svc.canManagePasskeys(httpx.AuthenticatedAt(r.Context())) {
			httpx.WriteJSON(w, http.StatusForbidden, httpx.ErrorBody{
				Code: "REAUTHENTICATION_REQUIRED", Message: "sign in again to manage passkeys",
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *Handler) passkeyList(w http.ResponseWriter, r *http.Request) {
	if h.svc.Passkeys == nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	pid, _ := httpx.PrincipalFromContext(r.Context())
	items, err := h.svc.listPasskeys(r.Context(), pid)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, struct {
		Items     []passkeySummary `json:"items"`
		CanManage bool             `json:"can_manage"`
	}{items, h.svc.canManagePasskeys(httpx.AuthenticatedAt(r.Context()))})
}

func (h *Handler) passkeyRegisterBegin(w http.ResponseWriter, r *http.Request) {
	pid, _ := httpx.PrincipalFromContext(r.Context())
	options, token, err := h.svc.beginPasskeyRegistration(r.Context(), pid)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"publicKey": options.Response, "ceremony_token": token})
}

func (h *Handler) passkeyRegisterFinish(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token      string          `json:"ceremony_token"`
		Name       string          `json:"name"`
		Credential json.RawMessage `json:"credential"`
	}
	if !httpx.DecodeJSONLimit(w, r, &in, passkeyBodyLimit) {
		return
	}
	pid, _ := httpx.PrincipalFromContext(r.Context())
	item, err := h.svc.finishPasskeyRegistration(r.Context(), pid, in.Token, in.Name, in.Credential)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, item)
}

func (h *Handler) passkeyRemove(w http.ResponseWriter, r *http.Request) {
	pid, _ := httpx.PrincipalFromContext(r.Context())
	keyID, err := uuid.Parse(chi.URLParam(r, "passkeyID"))
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	if err := h.svc.removePasskey(r.Context(), pid, keyID); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (p *Passkeys) loginCookie(token string, maxAge int) *http.Cookie {
	name := "mf_passkey_login"
	if p.secure {
		name = "__Host-" + name
	}
	return &http.Cookie{
		Name: name, Value: token, MaxAge: maxAge, Path: "/",
		HttpOnly: true, Secure: p.secure, SameSite: http.SameSiteStrictMode,
	}
}

func (h *Handler) passkeyLoginBegin(w http.ResponseWriter, r *http.Request) {
	options, token, err := h.svc.beginPasskeyLogin(r.Context())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	http.SetCookie(w, h.svc.Passkeys.loginCookie(token, int(passkeyTTL.Seconds())))
	httpx.WriteJSON(w, http.StatusOK, options)
}

func (h *Handler) passkeyLoginFinish(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Credential json.RawMessage `json:"credential"`
	}
	if !httpx.DecodeJSONLimit(w, r, &in, passkeyBodyLimit) {
		return
	}
	clearCookie := h.svc.Passkeys.loginCookie("", -1)
	loginCookie, err := r.Cookie(clearCookie.Name)
	if err != nil {
		writeInvalidPasskey(w)
		return
	}
	http.SetCookie(w, clearCookie)
	pair, err := h.svc.finishPasskeyLogin(r.Context(), loginCookie.Value, in.Credential)
	if errors.Is(err, ErrInvalidCredentials) {
		writeInvalidPasskey(w)
		return
	}
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, tokenResp{pair.Access, pair.Refresh, pair.ExpiresIn})
}

func writeInvalidPasskey(w http.ResponseWriter) {
	httpx.WriteJSON(w, http.StatusUnauthorized, httpx.ErrorBody{Code: "INVALID_CREDENTIALS", Message: "invalid credentials"})
}
