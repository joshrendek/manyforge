//go:build integration

package mailing_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/authz"
	"github.com/manyforge/manyforge/internal/mailing"
	"github.com/manyforge/manyforge/internal/platform/blob"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

type brandHarness struct {
	t      *testing.T
	router *chi.Mux
}

// newBrandHarness mounts the authenticated brand routes behind the real permission gates,
// selecting the acting principal per request, plus the principal-less public routes.
func newBrandHarness(t *testing.T, tdb *testdb.TestDB, svc *mailing.Service) *brandHarness {
	t.Helper()
	// The exact resolver main.go wires (authz.Resolve adapted to httpx.Permissions).
	resolve := func(ctx context.Context, tx pgx.Tx, principalID, businessID uuid.UUID) (httpx.Permissions, error) {
		return authz.Resolve(ctx, tx, principalID, businessID)
	}
	businessIDFromPath := func(r *http.Request) (uuid.UUID, error) { return uuid.Parse(chi.URLParam(r, "id")) }
	handler := mailing.NewHandler(svc)
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if raw := r.Header.Get("X-Test-Principal"); raw != "" {
				r = r.WithContext(httpx.WithPrincipal(r.Context(), uuid.MustParse(raw)))
			}
			next.ServeHTTP(w, r)
		})
	})
	router.Route("/api/v1", func(api chi.Router) {
		api.Group(func(readRouter chi.Router) {
			readRouter.Use(httpx.RequirePermission(tdb.App, resolve, authz.PermMailingRead, businessIDFromPath))
			handler.ReadRoutes(readRouter)
		})
		api.Group(func(writeRouter chi.Router) {
			writeRouter.Use(httpx.RequirePermission(tdb.App, resolve, authz.PermMailingWrite, businessIDFromPath))
			handler.WriteRoutes(writeRouter)
		})
		mailing.NewPublicHandler(svc, nil, nil, nil).PublicRoutes(api)
	})
	mailing.NewPublicHandler(svc, nil, nil, nil).RootRoutes(router)
	return &brandHarness{t: t, router: router}
}

func (h *brandHarness) do(principal uuid.UUID, method, target string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	h.t.Helper()
	req := httptest.NewRequest(method, target, bytes.NewReader(body))
	if principal != uuid.Nil {
		req.Header.Set("X-Test-Principal", principal.String())
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	return w
}

func (h *brandHarness) json(principal uuid.UUID, method, target string, v any) *httptest.ResponseRecorder {
	h.t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		h.t.Fatal(err)
	}
	return h.do(principal, method, target, raw, map[string]string{"Content-Type": "application/json"})
}

func (h *brandHarness) upload(principal uuid.UUID, target string, content []byte, declared string) *httptest.ResponseRecorder {
	h.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreatePart(map[string][]string{
		"Content-Disposition": {`form-data; name="file"; filename="logo"`},
		"Content-Type":        {declared},
	})
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err = part.Write(content); err != nil {
		h.t.Fatal(err)
	}
	if err = mw.Close(); err != nil {
		h.t.Fatal(err)
	}
	return h.do(principal, http.MethodPut, target, buf.Bytes(), map[string]string{"Content-Type": mw.FormDataContentType()})
}

func decodeBrand(t *testing.T, w *httptest.ResponseRecorder) mailing.Brand {
	t.Helper()
	var out mailing.Brand
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode brand: %v body=%s", err, w.Body.String())
	}
	return out
}

func testPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			img.Set(x, y, color.NRGBA{R: uint8(x), G: uint8(y), B: 0x80, A: 0xff})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestBrandLifecycleLogoAndIsolation(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	defer tdb.Close(ctx)
	store, err := blob.Open(ctx, "file://"+t.TempDir())
	if err != nil {
		t.Fatalf("open blob store: %v", err)
	}
	defer store.Close()
	a := seedMailingTenant(ctx, t, tdb)
	b := seedMailingTenant(ctx, t, tdb)
	svc := &mailing.Service{DB: tdb.App, Blob: store, PublicBaseURL: "https://mail.example.test/"}
	h := newBrandHarness(t, tdb, svc)
	brandPath := "/api/v1/businesses/" + a.businessID.String() + "/mailing/brand"

	if w := h.do(a.principalID, http.MethodGet, brandPath, nil, nil); w.Code != http.StatusNotFound {
		t.Fatalf("GET before create = %d body=%s", w.Code, w.Body.String())
	}

	put := map[string]any{
		"name":   "  Acme Co  ",
		"colors": map[string]string{"accent": "#FF8800", "background": ""},
	}
	w := h.json(a.principalID, http.MethodPut, brandPath, put)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT create = %d body=%s", w.Code, w.Body.String())
	}
	created := decodeBrand(t, w)
	if created.Name != "Acme Co" || created.Colors.Accent != "#ff8800" || created.Colors.Background != "#f4f6f8" ||
		created.FontStack != "system" || created.LogoWidth != 160 || created.Logo != nil || created.BusinessID != a.businessID {
		t.Fatalf("created brand = %+v", created)
	}
	w = h.do(a.principalID, http.MethodGet, brandPath, nil, nil)
	if w.Code != http.StatusOK || decodeBrand(t, w).ID != created.ID {
		t.Fatalf("GET after create = %d body=%s", w.Code, w.Body.String())
	}

	put["colors"] = map[string]string{"accent": "#123456", "surface": "#eeeeee"}
	put["font_stack"] = "serif"
	put["logo_width"] = 200
	w = h.json(a.principalID, http.MethodPut, brandPath, put)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT replace = %d body=%s", w.Code, w.Body.String())
	}
	replaced := decodeBrand(t, w)
	if replaced.ID != created.ID || replaced.Colors.Accent != "#123456" || replaced.Colors.Surface != "#eeeeee" ||
		replaced.FontStack != "serif" || replaced.LogoWidth != 200 || !replaced.UpdatedAt.After(created.UpdatedAt) {
		t.Fatalf("replaced brand = %+v (created %+v)", replaced, created)
	}

	for name, body := range map[string]map[string]any{
		"invalid color": {"name": "x", "colors": map[string]string{"text": "red"}},
		"bad font":      {"name": "x", "font_stack": "comic"},
		"empty name":    {"name": " "},
		"width":         {"name": "x", "logo_width": 12},
		"footer":        {"name": "x", "footer_markdown": strings.Repeat("a", 4097)},
	} {
		if w := h.json(a.principalID, http.MethodPut, brandPath, body); w.Code != http.StatusBadRequest {
			t.Errorf("PUT %s = %d body=%s, want 400", name, w.Code, w.Body.String())
		}
	}

	// Logo: real PNG succeeds and the natural width lands in logo_width.
	logo := testPNG(t, 320, 90)
	w = h.upload(a.principalID, brandPath+"/logo", logo, "application/octet-stream")
	if w.Code != http.StatusOK {
		t.Fatalf("PUT logo = %d body=%s", w.Code, w.Body.String())
	}
	withLogo := decodeBrand(t, w)
	sum := sha256.Sum256(logo)
	wantURL := "https://mail.example.test/m/b/" + created.ID.String() + "/logo?v=" + hex.EncodeToString(sum[:8])
	if withLogo.Logo == nil || withLogo.Logo.URL != wantURL || withLogo.Logo.ContentType != "image/png" || withLogo.LogoWidth != 240 {
		t.Fatalf("brand with logo = %+v", withLogo)
	}
	w = h.do(a.principalID, http.MethodGet, brandPath, nil, nil)
	if got := decodeBrand(t, w); got.Logo == nil || !strings.Contains(got.Logo.URL, "/m/b/"+created.ID.String()+"/logo?v=") {
		t.Fatalf("GET with logo = %+v", got)
	}

	// Public logo route: bytes, immutable caching, conditional 304, unknown id 404.
	logoPath := "/m/b/" + created.ID.String() + "/logo?v=ignored"
	w = h.do(uuid.Nil, http.MethodGet, logoPath, nil, nil)
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), logo) ||
		w.Header().Get("Content-Type") != "image/png" ||
		w.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" ||
		w.Header().Get("ETag") != `"`+hex.EncodeToString(sum[:])+`"` ||
		w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("public logo = %d headers=%v", w.Code, w.Header())
	}
	w = h.do(uuid.Nil, http.MethodGet, logoPath, nil, map[string]string{"If-None-Match": `"` + hex.EncodeToString(sum[:]) + `"`})
	if w.Code != http.StatusNotModified || w.Body.Len() != 0 {
		t.Fatalf("conditional logo = %d body=%d bytes", w.Code, w.Body.Len())
	}
	if w = h.do(uuid.Nil, http.MethodGet, "/m/b/"+uuid.NewString()+"/logo", nil, nil); w.Code != http.StatusNotFound {
		t.Fatalf("unknown logo = %d", w.Code)
	}
	if w = h.do(uuid.Nil, http.MethodGet, "/m/b/not-a-uuid/logo", nil, nil); w.Code != http.StatusNotFound {
		t.Fatalf("malformed logo id = %d", w.Code)
	}

	// Declared type is ignored: text bytes labelled image/png are rejected, nothing changes.
	w = h.upload(a.principalID, brandPath+"/logo", []byte("hello, this is not an image at all\n"), "image/png")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("text logo = %d body=%s", w.Code, w.Body.String())
	}
	if w = h.upload(a.principalID, brandPath+"/logo", testPNG(t, 2001, 10), "image/png"); w.Code != http.StatusBadRequest {
		t.Fatalf("oversized dimensions = %d body=%s", w.Code, w.Body.String())
	}
	if w = h.upload(a.principalID, brandPath+"/logo", bytes.Repeat([]byte{0x89}, (512<<10)+1), "image/png"); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body = %d body=%s", w.Code, w.Body.String())
	}
	if got := decodeBrand(t, h.do(a.principalID, http.MethodGet, brandPath, nil, nil)); got.Logo == nil || got.Logo.URL != wantURL {
		t.Fatalf("logo changed after rejected uploads: %+v", got)
	}

	// Public brand-by-key: uniform 200 envelope, null for unknown keys.
	list, err := svc.CreateList(ctx, a.principalID, a.businessID, mailing.ListInput{Name: "News"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := svc.CreateListKey(ctx, a.principalID, a.businessID, list.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	w = h.do(uuid.Nil, http.MethodGet, "/api/v1/mailing/public/"+key.PublishableKey+"/brand", nil, nil)
	var envelope struct {
		Brand *mailing.PublicBrand `json:"brand"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil || w.Code != http.StatusOK {
		t.Fatalf("public brand = %d body=%s err=%v", w.Code, w.Body.String(), err)
	}
	if envelope.Brand == nil || envelope.Brand.Name != "Acme Co" || envelope.Brand.Colors.Accent != "#123456" ||
		envelope.Brand.LogoURL == nil || *envelope.Brand.LogoURL != wantURL ||
		w.Header().Get("Cache-Control") != "public, max-age=300" || w.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("public brand = %+v headers=%v", envelope.Brand, w.Header())
	}
	w = h.do(uuid.Nil, http.MethodGet, "/api/v1/mailing/public/mlk_unknown/brand", nil, nil)
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"brand":null}` {
		t.Fatalf("unknown key brand = %d body=%s", w.Code, w.Body.String())
	}

	// Tenant isolation: another root's principal sees nothing and cannot write.
	if w = h.do(b.principalID, http.MethodGet, brandPath, nil, nil); w.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant GET = %d body=%s", w.Code, w.Body.String())
	}
	if w = h.json(b.principalID, http.MethodDelete, brandPath, nil); w.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant DELETE = %d", w.Code)
	}
	if _, err = svc.GetBrand(ctx, b.principalID, a.businessID); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("cross-tenant GetBrand error = %v, want not found", err)
	}

	// Deleting the logo clears the reference and the public route.
	w = h.do(a.principalID, http.MethodDelete, brandPath+"/logo", nil, nil)
	if w.Code != http.StatusOK || decodeBrand(t, w).Logo != nil {
		t.Fatalf("DELETE logo = %d body=%s", w.Code, w.Body.String())
	}
	if w = h.do(uuid.Nil, http.MethodGet, logoPath, nil, nil); w.Code != http.StatusNotFound {
		t.Fatalf("logo after delete = %d", w.Code)
	}
	if _, err = store.Get(ctx, blob.BrandLogoKey(a.businessID, a.businessID, created.ID)); err == nil {
		t.Fatal("logo object survived DELETE logo")
	}

	// Re-upload, then delete the brand: row, object, and public route all go away.
	if w = h.upload(a.principalID, brandPath+"/logo", logo, "image/png"); w.Code != http.StatusOK {
		t.Fatalf("re-upload = %d body=%s", w.Code, w.Body.String())
	}
	if w = h.do(a.principalID, http.MethodDelete, brandPath, nil, nil); w.Code != http.StatusNoContent {
		t.Fatalf("DELETE brand = %d body=%s", w.Code, w.Body.String())
	}
	if w = h.do(a.principalID, http.MethodGet, brandPath, nil, nil); w.Code != http.StatusNotFound {
		t.Fatalf("GET after delete = %d", w.Code)
	}
	if w = h.do(a.principalID, http.MethodDelete, brandPath, nil, nil); w.Code != http.StatusNotFound {
		t.Fatalf("second DELETE = %d", w.Code)
	}
	if w = h.do(uuid.Nil, http.MethodGet, logoPath, nil, nil); w.Code != http.StatusNotFound {
		t.Fatalf("logo after brand delete = %d", w.Code)
	}
	if _, err = store.Get(ctx, blob.BrandLogoKey(a.businessID, a.businessID, created.ID)); err == nil {
		t.Fatal("logo object survived DELETE brand")
	}
	if w = h.upload(a.principalID, brandPath+"/logo", logo, "image/png"); w.Code != http.StatusNotFound {
		t.Fatalf("logo upload without brand = %d", w.Code)
	}

	// Audit trail names every mutation.
	var actions []string
	rows, err := tdb.Super.Query(ctx, `SELECT action FROM audit_entry WHERE business_id=$1 AND target_type='mailing_brand' ORDER BY created_at, action`, a.businessID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var action string
		if err := rows.Scan(&action); err != nil {
			t.Fatal(err)
		}
		actions = append(actions, action)
	}
	rows.Close()
	for _, want := range []string{"mailing.brand.updated", "mailing.brand.logo_updated", "mailing.brand.logo_deleted", "mailing.brand.deleted"} {
		found := false
		for _, a := range actions {
			found = found || a == want
		}
		if !found {
			t.Errorf("audit missing %s in %v", want, actions)
		}
	}

	// Storage unconfigured: upload is a validation failure, not a crash.
	svc.Blob = nil
	if w = h.json(a.principalID, http.MethodPut, brandPath, map[string]any{"name": "Again"}); w.Code != http.StatusOK {
		t.Fatalf("PUT after delete = %d body=%s", w.Code, w.Body.String())
	}
	w = h.upload(a.principalID, brandPath+"/logo", logo, "image/png")
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "logo storage is not configured") {
		t.Fatalf("upload without store = %d body=%s", w.Code, w.Body.String())
	}
}
