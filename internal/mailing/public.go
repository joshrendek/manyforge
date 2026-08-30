package mailing

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/mail"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	mailrender "github.com/manyforge/manyforge/internal/mailing/render"
	"github.com/manyforge/manyforge/internal/platform/crypto"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/httpx"
	"github.com/manyforge/manyforge/internal/platform/notify"
	"github.com/manyforge/manyforge/internal/platform/ratelimit"
)

const (
	maxPublicMailingBytes int64 = 64 << 10
	confirmationTTL             = 48 * time.Hour
	signatureMaxSkew            = 5 * time.Minute
)

var errMailingRateLimited = errors.New("mailing: rate limited")

type publicListContext struct {
	listID, businessID, tenantRootID, keyID uuid.UUID
	doubleOptIn                             bool
	sealedSecret                            *string
}

// PublicSubscriptionInput carries normalized subscriber fields into the DEFINER boundary.
type PublicSubscriptionInput struct {
	Email            string
	FirstName        *string
	LastName         *string
	Attributes       map[string]any
	ConsentIP        string
	ConsentUserAgent string
	SkipConfirmation bool
}

// PublicSubscriptionResult is returned to authenticated S2S callers. Public-form callers receive
// a deliberately uniform acceptance body instead.
type PublicSubscriptionResult struct {
	SubscriberID uuid.UUID `json:"subscriber_id"`
	Created      bool      `json:"created"`
	Status       string    `json:"status"`
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func resolvePublicList(ctx context.Context, tx pgx.Tx, key string) (publicListContext, bool, error) {
	var out publicListContext
	err := tx.QueryRow(ctx, `
		SELECT list_id, business_id, tenant_root_id, double_opt_in, key_id, sealed_secret
		FROM mailing_public_list($1)`, key,
	).Scan(&out.listID, &out.businessID, &out.tenantRootID, &out.doubleOptIn, &out.keyID, &out.sealedSecret)
	if errors.Is(err, pgx.ErrNoRows) {
		return publicListContext{}, false, nil
	}
	return out, err == nil, err
}

// subscribeResolved performs the mutation in the caller's transaction. S2S uses this after
// verifying the request against the key resolved in that same transaction, closing the
// verify/revoke race between authentication and use.
func (s *Service) subscribeResolved(ctx context.Context, tx pgx.Tx, list publicListContext, in PublicSubscriptionInput, s2s bool) (PublicSubscriptionResult, string, string, error) {
	email, err := normalizeEmail(in.Email)
	if err != nil {
		return PublicSubscriptionResult{}, "", "", err
	}
	attrs, err := jsonObject(in.Attributes)
	if err != nil {
		return PublicSubscriptionResult{}, "", "", err
	}
	in.ConsentUserAgent = strings.ToValidUTF8(in.ConsentUserAgent, "�")

	var rawConfirmation string
	var hash []byte
	var expires *time.Time
	if list.doubleOptIn && !in.SkipConfirmation {
		if s.Tokens == nil {
			return PublicSubscriptionResult{}, "", "", errors.New("mailing: token codec unavailable")
		}
		rawConfirmation, hash, err = s.Tokens.NewConfirmation()
		if err != nil {
			return PublicSubscriptionResult{}, "", "", err
		}
		t := s.now().Add(confirmationTTL)
		expires = &t
	}

	var ip any
	if strings.TrimSpace(in.ConsentIP) != "" && !s2s {
		ip = strings.TrimSpace(in.ConsentIP)
	}
	query := `SELECT subscriber_id, created, subscriber_status::text
		FROM mailing_public_subscribe($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`
	args := []any{list.keyID, list.listID, list.businessID, list.tenantRootID, email,
		cleanOptional(in.FirstName), cleanOptional(in.LastName), attrs, ip,
		nullIfEmpty(in.ConsentUserAgent), hash, expires}
	if s2s {
		query = `SELECT subscriber_id, created, subscriber_status::text
			FROM mailing_s2s_subscribe($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`
		args = []any{list.keyID, list.listID, list.businessID, list.tenantRootID, email,
			cleanOptional(in.FirstName), cleanOptional(in.LastName), attrs,
			in.SkipConfirmation, hash, expires}
	}
	var out PublicSubscriptionResult
	if err := tx.QueryRow(ctx, query, args...).Scan(&out.SubscriberID, &out.Created, &out.Status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PublicSubscriptionResult{}, "", email, nil
		}
		return PublicSubscriptionResult{}, "", email, err
	}
	return out, rawConfirmation, email, nil
}

func (s *Service) sendConfirmation(ctx context.Context, businessID uuid.UUID, email, raw string) {
	logger := s.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if s.Providers == nil || s.Renderer == nil {
		logger.ErrorContext(ctx, "mailing confirmation send failed", "err", "delivery is not configured")
		return
	}
	link := strings.TrimSuffix(s.PublicBaseURL, "/") + "/m/confirm/" + url.PathEscape(raw)
	profile, err := s.resolveBusinessProfile(ctx, businessID)
	if err != nil {
		logger.ErrorContext(ctx, "mailing confirmation profile resolution failed", "err", err)
		return
	}
	deliverer, err := s.Providers.Resolve(ctx, profile.provider)
	if err != nil {
		logger.ErrorContext(ctx, "mailing confirmation provider resolution failed", "err", err)
		return
	}
	rendered, err := s.Renderer.RenderInput(mailrender.Input{
		BodyMarkdown: "# Confirm your subscription\n\n[Confirm your subscription](" + link + ")",
		FromName:     profile.fromName, PostalAddress: stringValue(profile.postalAddress),
	}, mailrender.Variables{Email: email, UnsubscribeURL: "#", ListName: "Mailing list"}, mailrender.Tracking{})
	if err != nil {
		logger.ErrorContext(ctx, "mailing confirmation render failed", "err", err)
		return
	}
	message := notify.Mail{From: (&mail.Address{Name: profile.fromName, Address: profile.provider.FromEmail}).String(),
		To: email, Subject: "Confirm your subscription", BodyText: rendered.Text,
		BodyHTML: rendered.HTML, MessageID: uuid.New().String() + "@" + safeMessageDomain(s.MessageDomain),
		EnvelopeFrom: profile.provider.FromEmail}
	if profile.replyTo != nil {
		message.ReplyTo = *profile.replyTo
	}
	_, err = deliverer.Send(ctx, message)
	if err != nil {
		logger.ErrorContext(ctx, "mailing confirmation send failed", "err", err)
	}
}

func nullIfEmpty(v string) any {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	return v
}

// PublicHandler serves principal-less list signup, signed S2S, and root confirmation/unsubscribe.
type PublicHandler struct {
	Service  *Service
	Logger   *slog.Logger
	Sealer   *crypto.Sealer
	ClientIP func(*http.Request) string
	Now      func() time.Time
	PerKey   ratelimit.Limiter
	// IngestLimit is applied inside RootRoutes because /m/* lives outside the shared /api/v1
	// ingress group. Production supplies the same trusted-proxy-aware limiter used by that group.
	IngestLimit func(http.Handler) http.Handler
	maxBytes    int64
}

// NewPublicHandler builds the principal-less mailing ingress and tracking handler.
func NewPublicHandler(svc *Service, logger *slog.Logger, sealer *crypto.Sealer, clientIP func(*http.Request) string) *PublicHandler {
	return &PublicHandler{Service: svc, Logger: logger, Sealer: sealer, ClientIP: clientIP, Now: time.Now, maxBytes: maxPublicMailingBytes}
}

// PublicRoutes mounts public-form and HMAC-authenticated S2S routes under /api/v1.
func (h *PublicHandler) PublicRoutes(r chi.Router) {
	r.Post("/mailing/public/{key}/subscribe", h.publicSubscribe)
	r.Options("/mailing/public/{key}/subscribe", h.publicPreflight)
	r.Post("/mailing/s2s/{key}/subscribers", h.s2sSubscribe)
	r.Delete("/mailing/s2s/{key}/subscribers/{email}", h.s2sUnsubscribe)
}

// mailingCORS is intentionally open: hosted forms may run on arbitrary tenant sites, these
// routes take no cookie or bearer credentials, and they return no tenant data.
func mailingCORS(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	w.Header().Set("Access-Control-Max-Age", "86400")
	w.Header().Set("Vary", "Origin")
}

func (h *PublicHandler) publicPreflight(w http.ResponseWriter, _ *http.Request) {
	mailingCORS(w)
	w.WriteHeader(http.StatusNoContent)
}

type publicSubscribeBody struct {
	Email            string         `json:"email"`
	FirstName        *string        `json:"first_name"`
	LastName         *string        `json:"last_name"`
	Attributes       map[string]any `json:"attributes"`
	SkipConfirmation bool           `json:"skip_confirmation"`
	Website          string         `json:"website"`
}

func (h *PublicHandler) readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	maxBytes := h.maxBytes
	if maxBytes <= 0 {
		maxBytes = maxPublicMailingBytes
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			httpx.WriteJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "payload too large"})
		} else {
			httpx.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		}
		return nil, false
	}
	return raw, true
}

func (h *PublicHandler) publicSubscribe(w http.ResponseWriter, r *http.Request) {
	mailingCORS(w)
	raw, ok := h.readBody(w, r)
	if !ok {
		return
	}
	var body publicSubscribeBody
	isJSON := strings.Contains(strings.ToLower(r.Header.Get("Content-Type")), "application/json")
	if isJSON {
		if err := json.Unmarshal(raw, &body); err != nil {
			httpx.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
			return
		}
	} else {
		values, err := url.ParseQuery(string(raw))
		if err != nil {
			httpx.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
			return
		}
		body.Email = values.Get("email")
		body.FirstName = stringPointer(values.Get("first_name"))
		body.LastName = stringPointer(values.Get("last_name"))
		body.Website = values.Get("website")
		if body.Website == "" {
			body.Website = values.Get("_gotcha")
		}
	}

	if body.Website == "" {
		ip := ""
		if h.ClientIP != nil {
			ip = h.ClientIP(r)
		}
		_, err := h.subscribePublic(r.Context(), chi.URLParam(r, "key"), PublicSubscriptionInput{
			Email: body.Email, FirstName: body.FirstName, LastName: body.LastName,
			Attributes: body.Attributes, ConsentIP: ip, ConsentUserAgent: r.UserAgent(),
		})
		if err != nil {
			if errors.Is(err, errMailingRateLimited) {
				httpx.WriteJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate_limited"})
				return
			}
			if errors.Is(err, errs.ErrValidation) {
				httpx.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
				return
			}
			h.logger().ErrorContext(r.Context(), "mailing public subscribe failed", "err", err)
			httpx.WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
	}
	if !isJSON {
		http.Redirect(w, r, "/m/s/"+url.PathEscape(chi.URLParam(r, "key"))+"?state=check-inbox", http.StatusSeeOther)
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, map[string]bool{"accepted": true})
}

func (h *PublicHandler) subscribePublic(ctx context.Context, key string, in PublicSubscriptionInput) (PublicSubscriptionResult, error) {
	var result PublicSubscriptionResult
	var rawConfirmation, email string
	var confirmationBusinessID uuid.UUID
	err := h.Service.DB.WithTx(ctx, func(tx pgx.Tx) error {
		list, found, err := resolvePublicList(ctx, tx, key)
		if err != nil || !found {
			return err
		}
		if h.PerKey != nil && !h.PerKey.Allow(list.keyID.String()) {
			return errMailingRateLimited
		}
		confirmationBusinessID = list.businessID
		result, rawConfirmation, email, err = h.Service.subscribeResolved(ctx, tx, list, in, false)
		return err
	})
	if err != nil {
		return PublicSubscriptionResult{}, err
	}
	if result.Status == "pending" && rawConfirmation != "" {
		h.Service.sendConfirmation(ctx, confirmationBusinessID, email, rawConfirmation)
	}
	return result, nil
}

func stringPointer(v string) *string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return &v
}

func (h *PublicHandler) s2sSubscribe(w http.ResponseWriter, r *http.Request) {
	raw, ok := h.readBody(w, r)
	if !ok {
		return
	}
	var body publicSubscribeBody
	if err := json.Unmarshal(raw, &body); err != nil {
		httpx.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	var result PublicSubscriptionResult
	var rawConfirmation, email string
	var confirmationBusinessID uuid.UUID
	authorized, err := h.withVerifiedS2S(r, raw, func(list publicListContext, tx pgx.Tx) error {
		var subErr error
		confirmationBusinessID = list.businessID
		result, rawConfirmation, email, subErr = h.Service.subscribeResolved(r.Context(), tx, list, PublicSubscriptionInput{
			Email: body.Email, FirstName: body.FirstName, LastName: body.LastName,
			Attributes: body.Attributes, SkipConfirmation: body.SkipConfirmation,
		}, true)
		return subErr
	})
	if err != nil {
		if errors.Is(err, errMailingRateLimited) {
			httpx.WriteJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate_limited"})
			return
		}
		if errors.Is(err, errs.ErrValidation) {
			httpx.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
			return
		}
		h.logger().ErrorContext(r.Context(), "mailing s2s subscribe failed", "err", err)
		httpx.WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if !authorized {
		h.unauthorized(w)
		return
	}
	if result.Status == "pending" && rawConfirmation != "" {
		// The signed S2S path resolves the same verified business profile as public signup.
		// Send failure is intentionally logged only; subscription response semantics stay stable.
		h.Service.sendConfirmation(r.Context(), confirmationBusinessID, email, rawConfirmation)
	}
	status := http.StatusOK
	if result.Created {
		status = http.StatusCreated
	}
	httpx.WriteJSON(w, status, result)
}

func (h *PublicHandler) s2sUnsubscribe(w http.ResponseWriter, r *http.Request) {
	authorized, err := h.withVerifiedS2S(r, nil, func(list publicListContext, tx pgx.Tx) error {
		rawEmail, err := url.PathUnescape(chi.URLParam(r, "email"))
		if err != nil {
			return validation("invalid email")
		}
		email, err := normalizeEmail(rawEmail)
		if err != nil {
			return err
		}
		var rows int
		return tx.QueryRow(r.Context(), `SELECT mailing_s2s_unsubscribe($1,$2)`, list.listID, email).Scan(&rows)
	})
	if err != nil {
		if errors.Is(err, errMailingRateLimited) {
			httpx.WriteJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate_limited"})
			return
		}
		if errors.Is(err, errs.ErrValidation) {
			httpx.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
			return
		}
		h.logger().ErrorContext(r.Context(), "mailing s2s unsubscribe failed", "err", err)
		httpx.WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if !authorized {
		h.unauthorized(w)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *PublicHandler) withVerifiedS2S(r *http.Request, raw []byte, fn func(publicListContext, pgx.Tx) error) (bool, error) {
	var authorized bool
	err := h.Service.DB.WithTx(r.Context(), func(tx pgx.Tx) error {
		list, found, err := resolvePublicList(r.Context(), tx, chi.URLParam(r, "key"))
		if err != nil || !found || list.sealedSecret == nil || h.Sealer == nil {
			return err
		}
		secret, err := h.Sealer.Open(*list.sealedSecret)
		if err != nil {
			return nil
		}
		now := time.Now()
		if h.Now != nil {
			now = h.Now()
		}
		if verifyMailingSignature(r.Header.Get("X-Mailing-Timestamp"), r.Header.Get("X-Mailing-Signature"), string(secret), r.Method, r.URL.EscapedPath(), raw, now) != nil {
			return nil
		}
		authorized = true
		if h.PerKey != nil && !h.PerKey.Allow(list.keyID.String()) {
			return errMailingRateLimited
		}
		return fn(list, tx)
	})
	return authorized, err
}

func mailingSigningString(timestamp, method, path string, body []byte) []byte {
	head := timestamp + "." + method + "." + path + "."
	out := make([]byte, 0, len(head)+len(body))
	out = append(out, head...)
	return append(out, body...)
}

func verifyMailingSignature(timestamp, signature, secret, method, path string, body []byte, now time.Time) error {
	ts, err := strconv.ParseInt(strings.TrimSpace(timestamp), 10, 64)
	if err != nil || ts < now.Add(-signatureMaxSkew).Unix() || ts > now.Add(signatureMaxSkew).Unix() {
		return errors.New("mailing: invalid signature timestamp")
	}
	signature = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(signature), "v1="))
	got, err := hex.DecodeString(signature)
	if err != nil {
		return errors.New("mailing: invalid signature encoding")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(mailingSigningString(strconv.FormatInt(ts, 10), method, path, body))
	if !hmac.Equal(got, mac.Sum(nil)) {
		return errors.New("mailing: signature mismatch")
	}
	return nil
}

func (h *PublicHandler) unauthorized(w http.ResponseWriter) {
	httpx.WriteJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
}

func (h *PublicHandler) logger() *slog.Logger {
	if h.Logger != nil {
		return h.Logger
	}
	return slog.Default()
}
