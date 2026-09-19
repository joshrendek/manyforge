package mailing

import (
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

type brandBody struct {
	Name           string      `json:"name"`
	Colors         BrandColors `json:"colors"`
	FontStack      string      `json:"font_stack"`
	FooterMarkdown string      `json:"footer_markdown"`
	LogoWidth      *int        `json:"logo_width"`
}

func (b *brandBody) input() *BrandInput {
	if b == nil {
		return nil
	}
	return &BrandInput{Name: b.Name, Colors: b.Colors, FontStack: b.FontStack, FooterMarkdown: b.FooterMarkdown, LogoWidth: b.LogoWidth}
}

func (h *Handler) getBrand(w http.ResponseWriter, r *http.Request) {
	pid, ids, ok := requestIDs(w, r, "id")
	if !ok {
		return
	}
	v, err := h.svc.GetBrand(r.Context(), pid, ids[0])
	write(w, r, http.StatusOK, v, err)
}
func (h *Handler) putBrand(w http.ResponseWriter, r *http.Request) {
	pid, ids, ok := requestIDs(w, r, "id")
	if !ok {
		return
	}
	var b brandBody
	if !httpx.DecodeJSON(w, r, &b) {
		return
	}
	v, err := h.svc.PutBrand(r.Context(), pid, ids[0], *b.input())
	write(w, r, http.StatusOK, v, err)
}
func (h *Handler) deleteBrand(w http.ResponseWriter, r *http.Request) {
	pid, ids, ok := requestIDs(w, r, "id")
	if !ok {
		return
	}
	if err := h.svc.DeleteBrand(r.Context(), pid, ids[0]); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	noContent(w)
}

// putBrandLogo reads a multipart form with a single `file` part, mirroring the CSV
// import handler. The part's declared Content-Type is ignored; the service sniffs the
// bytes. The body cap allows a small multipart envelope on top of the logo cap; a part
// over the logo cap is refused with 413 before any storage write.
func (h *Handler) putBrandLogo(w http.ResponseWriter, r *http.Request) {
	pid, ids, ok := requestIDs(w, r, "id")
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBrandLogoBytes+(64<<10))
	mr, err := r.MultipartReader()
	if err != nil {
		httpx.WriteError(w, r, validation("multipart form required"))
		return
	}
	var file []byte
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeMultipartError(w, r, err, "invalid multipart form")
			return
		}
		raw, err := io.ReadAll(io.LimitReader(part, maxBrandLogoBytes+1))
		closeErr := part.Close()
		if err != nil {
			writeMultipartError(w, r, err, "cannot read multipart field")
			return
		}
		if closeErr != nil {
			httpx.WriteError(w, r, validation("cannot close multipart field"))
			return
		}
		if part.FormName() == "file" {
			if int64(len(raw)) > maxBrandLogoBytes {
				httpx.WriteJSON(w, http.StatusRequestEntityTooLarge, httpx.ErrorBody{Code: "PAYLOAD_TOO_LARGE", Message: "payload too large"})
				return
			}
			file = raw
		}
	}
	if len(file) == 0 {
		httpx.WriteError(w, r, validation("file is required"))
		return
	}
	v, err := h.svc.PutBrandLogo(r.Context(), pid, ids[0], file)
	write(w, r, http.StatusOK, v, err)
}

// writeMultipartError maps a body-cap overflow to 413 and everything else to 400.
func writeMultipartError(w http.ResponseWriter, r *http.Request, err error, msg string) {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		httpx.WriteJSON(w, http.StatusRequestEntityTooLarge, httpx.ErrorBody{Code: "PAYLOAD_TOO_LARGE", Message: "payload too large"})
		return
	}
	httpx.WriteError(w, r, validation(msg))
}
func (h *Handler) deleteBrandLogo(w http.ResponseWriter, r *http.Request) {
	pid, ids, ok := requestIDs(w, r, "id")
	if !ok {
		return
	}
	v, err := h.svc.DeleteBrandLogo(r.Context(), pid, ids[0])
	write(w, r, http.StatusOK, v, err)
}

// brandLogo serves logo bytes to email clients. The brand id is the only capability
// needed; unknown ids and logo-less brands are indistinguishable 404s. The response is
// immutable-cacheable because the ?v= in the embedded URL changes with the content.
func (h *PublicHandler) brandLogo(w http.ResponseWriter, r *http.Request) {
	brandID, err := uuid.Parse(chi.URLParam(r, "bid"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	content, contentType, sha, err := h.Service.PublicBrandLogo(r.Context(), brandID)
	if err != nil {
		if errors.Is(err, errs.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		h.logger().ErrorContext(r.Context(), "mailing brand logo read failed", "err", err)
		httpx.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "temporarily unavailable"})
		return
	}
	etag := `"` + hex.EncodeToString(sha) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(content)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

// publicBrand serves the brand behind a publishable key to hosted signup pages. The
// body shape is uniform for unknown keys and brand-less businesses.
func (h *PublicHandler) publicBrand(w http.ResponseWriter, r *http.Request) {
	mailingCORS(w)
	brand, err := h.Service.PublicBrandForKey(r.Context(), chi.URLParam(r, "key"))
	if err != nil {
		h.logger().ErrorContext(r.Context(), "mailing public brand lookup failed", "err", err)
		httpx.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "temporarily unavailable"})
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=300")
	httpx.WriteJSON(w, http.StatusOK, map[string]*PublicBrand{"brand": brand})
}
