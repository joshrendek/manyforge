package githubapp

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

func sign256(secret, body []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

type recordInstalls struct{ upserted, deleted []int64 }

func (r *recordInstalls) UpsertFromEvent(ctx context.Context, id int64, l, a string) error {
	r.upserted = append(r.upserted, id)
	return nil
}
func (r *recordInstalls) MarkDeleted(ctx context.Context, id int64) error {
	r.deleted = append(r.deleted, id)
	return nil
}
func (r *recordInstalls) SetSuspended(ctx context.Context, id int64, s bool) error { return nil }
func (r *recordInstalls) Link(ctx context.Context, id int64, biz, agent uuid.UUID) error {
	return nil
}

func newWebhookHandler(store appConfigStore, installs installOps) *Handler {
	return &Handler{Store: store, Installs: installs, Now: func() time.Time { return time.Unix(1_700_000_000, 0) }}
}

func TestWebhookRejectsBadSignature(t *testing.T) {
	h := newWebhookHandler(&stubStore{cfg: AppConfig{AppID: 5, WebhookSecret: "whsec"}}, &recordInstalls{})
	r := chi.NewRouter()
	h.WebhookRoutes(r)
	body := []byte(`{"action":"created","installation":{"id":9}}`)
	req := httptest.NewRequest(http.MethodPost, "/github/webhook", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "installation")
	req.Header.Set("X-GitHub-Hook-Installation-Target-ID", "5")
	req.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestWebhookInstallationCreatedUpserts(t *testing.T) {
	rec := &recordInstalls{}
	h := newWebhookHandler(&stubStore{cfg: AppConfig{AppID: 5, WebhookSecret: "whsec"}}, rec)
	r := chi.NewRouter()
	h.WebhookRoutes(r)
	body := []byte(`{"action":"created","installation":{"id":9,"account":{"login":"bluescripts-net","type":"Organization"}}}`)
	req := httptest.NewRequest(http.MethodPost, "/github/webhook", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "installation")
	req.Header.Set("X-GitHub-Hook-Installation-Target-ID", "5")
	req.Header.Set("X-Hub-Signature-256", sign256([]byte("whsec"), body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", w.Code)
	}
	if len(rec.upserted) != 1 || rec.upserted[0] != 9 {
		t.Fatalf("upserted = %v", rec.upserted)
	}
}
