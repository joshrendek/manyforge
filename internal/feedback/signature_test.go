package feedback

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"
	"time"
)

func sign(t int64, secret, method, path string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(feedbackSigningString(t, method, path, body))
	return fmt.Sprintf("t=%d,v1=%s", t, hex.EncodeToString(mac.Sum(nil)))
}

func TestVerifyFeedbackSignature(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	secret, method, path := "fbs_abc", "POST", "/feedback/public/fbk_x/posts"
	body := []byte(`{"title":"hi","idempotency_key":"k1"}`)

	t.Run("valid", func(t *testing.T) {
		h := sign(now.Unix(), secret, method, path, body)
		if err := verifyFeedbackSignature(h, secret, method, path, body, now); err != nil {
			t.Fatalf("want verified, got %v", err)
		}
	})
	t.Run("absent → errNoSignature", func(t *testing.T) {
		if err := verifyFeedbackSignature("", secret, method, path, body, now); !errors.Is(err, errNoSignature) {
			t.Fatalf("want errNoSignature, got %v", err)
		}
	})
	t.Run("tampered body → error", func(t *testing.T) {
		h := sign(now.Unix(), secret, method, path, body)
		if err := verifyFeedbackSignature(h, secret, method, path, []byte(`{"title":"HACKED"}`), now); err == nil || errors.Is(err, errNoSignature) {
			t.Fatalf("want bad-sig error, got %v", err)
		}
	})
	t.Run("wrong path → error (method+path binding)", func(t *testing.T) {
		h := sign(now.Unix(), secret, method, path, body)
		if err := verifyFeedbackSignature(h, secret, method, "/feedback/public/fbk_x/posts/OTHER/votes", body, now); err == nil || errors.Is(err, errNoSignature) {
			t.Fatalf("want error for path mismatch, got %v", err)
		}
	})
	t.Run("expired → error", func(t *testing.T) {
		h := sign(now.Unix()-301, secret, method, path, body)
		if err := verifyFeedbackSignature(h, secret, method, path, body, now); err == nil || errors.Is(err, errNoSignature) {
			t.Fatalf("want expired error, got %v", err)
		}
	})
	t.Run("future skew → error", func(t *testing.T) {
		h := sign(now.Unix()+301, secret, method, path, body)
		if err := verifyFeedbackSignature(h, secret, method, path, body, now); err == nil || errors.Is(err, errNoSignature) {
			t.Fatalf("want future-skew error, got %v", err)
		}
	})
	t.Run("malformed header → error", func(t *testing.T) {
		if err := verifyFeedbackSignature("garbage", secret, method, path, body, now); err == nil || errors.Is(err, errNoSignature) {
			t.Fatalf("want malformed error, got %v", err)
		}
	})
}
