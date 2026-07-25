package telemetry

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestNewPublishableKey_ShapeAndUniqueness(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		k, err := newPublishableKey()
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		if !strings.HasPrefix(k, keyPrefix) {
			t.Fatalf("missing %q prefix: %q", keyPrefix, k)
		}
		if len(k) != len(keyPrefix)+keyBodyLen {
			t.Fatalf("unexpected length %d for %q", len(k), k)
		}
		if seen[k] {
			t.Fatalf("duplicate key minted: %q", k)
		}
		seen[k] = true
	}
}

func TestNewSecret_DistinctPrefixFromPublishableKey(t *testing.T) {
	s, err := newSecret()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if !strings.HasPrefix(s, secretPrefix) {
		t.Fatalf("missing %q prefix: %q", secretPrefix, s)
	}
	// The prefixes must differ, or a secret pasted into a publishable-key config slot (or vice
	// versa) would be indistinguishable to an operator.
	if strings.HasPrefix(s, keyPrefix) {
		t.Fatal("secret and publishable key share a prefix")
	}
}

func TestClampOccurredAt(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		name     string
		occurred time.Time
		want     time.Time
	}{
		{"far future is clamped to now", now.Add(48 * time.Hour), now},
		{"just past the skew window is clamped", now.Add(maxFutureSkew + time.Second), now},
		{"inside the skew window is preserved", now.Add(time.Minute), now.Add(time.Minute)},
		{"past is preserved", now.Add(-time.Hour), now.Add(-time.Hour)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clampOccurredAt(tc.occurred, now); !got.Equal(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestTooOld(t *testing.T) {
	now := time.Now().UTC()
	if !tooOld(time.Time{}, now) {
		t.Fatal("zero timestamp should be treated as too old (unset field, not 1970)")
	}
	if !tooOld(now.Add(-maxPastAge-time.Hour), now) {
		t.Fatal("event beyond the retention window should be too old")
	}
	if tooOld(now.Add(-time.Hour), now) {
		t.Fatal("recent event should be accepted")
	}
}

// A single stale or malformed event must not discard the rest of the batch.
func TestSanitizeAnalytics_DropsBadEventsNotTheBatch(t *testing.T) {
	now := time.Now().UTC()
	in := []AnalyticsEvent{
		{OccurredAt: now.Add(-time.Minute), Name: "pageview"},
		{OccurredAt: now.Add(-maxPastAge - time.Hour), Name: "ancient"},
		{OccurredAt: now.Add(-time.Minute), Name: ""}, // missing name
		{OccurredAt: now.Add(72 * time.Hour), Name: "future"},
	}
	out, dropped := sanitizeAnalytics(in, now)
	if dropped != 2 {
		t.Fatalf("expected 2 dropped, got %d", dropped)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 kept, got %d", len(out))
	}
	for _, e := range out {
		if e.OccurredAt.After(now) {
			t.Fatalf("future timestamp survived clamping: %v", e.OccurredAt)
		}
	}
}

func TestSanitizeAnalytics_CapsBatchSize(t *testing.T) {
	now := time.Now().UTC()
	in := make([]AnalyticsEvent, maxBatchEvents+500)
	for i := range in {
		in[i] = AnalyticsEvent{OccurredAt: now, Name: "e"}
	}
	out, _ := sanitizeAnalytics(in, now)
	if len(out) != maxBatchEvents {
		t.Fatalf("batch not capped: got %d, want %d", len(out), maxBatchEvents)
	}
}

func TestSanitizeCrash_RequiresPlatformAndSignature(t *testing.T) {
	now := time.Now().UTC()
	in := []CrashEvent{
		{OccurredAt: now, Platform: "ios", Signature: "SIGSEGV@main"},
		{OccurredAt: now, Platform: "", Signature: "SIGSEGV@main"},
		{OccurredAt: now, Platform: "ios", Signature: ""},
	}
	out, dropped := sanitizeCrash(in, now)
	if len(out) != 1 || dropped != 2 {
		t.Fatalf("got %d kept / %d dropped, want 1/2", len(out), dropped)
	}
}

// ---------------------------------------------------------------------------
// Signature
// ---------------------------------------------------------------------------

func sign(secret string, ts int64, method, target string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(telemetrySigningString(ts, method, target, body))
	return fmt.Sprintf("t=%d,v1=%s", ts, hex.EncodeToString(mac.Sum(nil)))
}

func TestVerifyTelemetrySignature(t *testing.T) {
	const secret = "mfs_testsecret"
	now := time.Now()
	body := []byte(`{"analytics":[{"name":"pageview"}]}`)
	target := "/api/v1/telemetry/ingest/mfk_abc"

	t.Run("valid signature verifies", func(t *testing.T) {
		h := sign(secret, now.Unix(), http.MethodPost, target, body)
		if err := verifyTelemetrySignature(h, secret, http.MethodPost, target, body, now); err != nil {
			t.Fatalf("expected valid: %v", err)
		}
	})

	t.Run("absent header is reported distinctly", func(t *testing.T) {
		err := verifyTelemetrySignature("", secret, http.MethodPost, target, body, now)
		if !errors.Is(err, errNoSignature) {
			t.Fatalf("want errNoSignature, got %v", err)
		}
	})

	t.Run("body tampering is rejected", func(t *testing.T) {
		h := sign(secret, now.Unix(), http.MethodPost, target, body)
		err := verifyTelemetrySignature(h, secret, http.MethodPost, target,
			[]byte(`{"analytics":[{"name":"tampered"}]}`), now)
		if err == nil {
			t.Fatal("tampered body accepted")
		}
	})

	// Signing the bare path instead of the full request-target is the single most common
	// integration mistake; it must fail loudly rather than silently pass.
	t.Run("target must include the /api/v1 prefix", func(t *testing.T) {
		h := sign(secret, now.Unix(), http.MethodPost, "/telemetry/ingest/mfk_abc", body)
		if err := verifyTelemetrySignature(h, secret, http.MethodPost, target, body, now); err == nil {
			t.Fatal("signature over the bare path was accepted")
		}
	})

	t.Run("replay outside the skew window is rejected", func(t *testing.T) {
		old := now.Add(-sigMaxSkew - time.Minute)
		h := sign(secret, old.Unix(), http.MethodPost, target, body)
		if err := verifyTelemetrySignature(h, secret, http.MethodPost, target, body, now); err == nil {
			t.Fatal("expired signature accepted")
		}
	})

	t.Run("wrong secret is rejected", func(t *testing.T) {
		h := sign("mfs_other", now.Unix(), http.MethodPost, target, body)
		if err := verifyTelemetrySignature(h, secret, http.MethodPost, target, body, now); err == nil {
			t.Fatal("signature under the wrong secret accepted")
		}
	})

	t.Run("malformed header is rejected", func(t *testing.T) {
		for _, h := range []string{"garbage", "t=abc,v1=ff", "v1=ff", "t=1"} {
			if err := verifyTelemetrySignature(h, secret, http.MethodPost, target, body, now); err == nil {
				t.Fatalf("malformed header %q accepted", h)
			}
		}
	})
}

func TestValidKind(t *testing.T) {
	if !validKind(KindAnalytics) || !validKind(KindCrash) {
		t.Fatal("known kinds should validate")
	}
	if validKind("") || validKind("metrics") {
		t.Fatal("unknown kinds should be rejected")
	}
}

func TestClampLimit(t *testing.T) {
	if got := clampLimit(0); got != 50 {
		t.Fatalf("default limit: got %d", got)
	}
	if got := clampLimit(10_000_000); got != maxClientsPerPage {
		t.Fatalf("limit not capped: got %d", got)
	}
	if got := clampLimit(10); got != 10 {
		t.Fatalf("valid limit altered: got %d", got)
	}
}
