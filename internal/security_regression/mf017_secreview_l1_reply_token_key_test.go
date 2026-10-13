// Finding: SECREVIEW-L1 — reply-token key validation permitted weak keys, and an
// empty (publicly known) key outside production. HMAC under an empty key signs and
// verifies normally, so anyone could mint a reply token for a known ticket UUID.
//
// Fix: ValidateServe requires >= 32 bytes whenever the key is set (every
// environment); an unset non-production key is replaced by a random ephemeral key
// on the serve path; Sign/VerifyReplyToken never sign/verify under an empty key.
package security_regression

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/config"
	"github.com/manyforge/manyforge/internal/ticketing"
)

func TestSecreviewL1_ShortReplyTokenKeyRejectedEverywhere(t *testing.T) {
	for _, env := range []string{"production", "development"} {
		cfg := config.Config{Environment: env, InboundReplyTokenSecret: []byte("short-key")}
		if err := cfg.ValidateServe(); err == nil {
			t.Errorf("%s: ValidateServe accepted a 9-byte reply-token key", env)
		}
	}
}

func TestSecreviewL1_DevServeNeverUsesEmptyKey(t *testing.T) {
	cfg := config.Config{Environment: "development"}
	if err := cfg.ValidateServe(); err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.EnsureReplyTokenKey(); err != nil {
		t.Fatal(err)
	}
	if len(bytes.TrimSpace(cfg.InboundReplyTokenSecret)) < config.MinReplyTokenKeyBytes {
		t.Fatalf("dev serve key length %d, want >= %d", len(cfg.InboundReplyTokenSecret), config.MinReplyTokenKeyBytes)
	}
}

func TestSecreviewL1_EmptyKeyTokenNeverVerifies(t *testing.T) {
	tid := uuid.New()
	mac := hmac.New(sha256.New, nil)
	mac.Write(tid[:])
	enc := base64.RawURLEncoding
	forged := enc.EncodeToString(tid[:]) + "." + enc.EncodeToString(mac.Sum(nil)[:16])
	if _, ok := ticketing.VerifyReplyToken(forged, nil); ok {
		t.Fatal("a token forged with the public empty key verified")
	}
	if ticketing.SignReplyToken(tid, nil) == forged {
		t.Fatal("SignReplyToken with an empty key emitted the public-key token")
	}
}

// Source pin: the serve path must install the key after ValidateServe and before
// any service is wired with it.
func TestPin_SecreviewL1ServePathEnsuresKey(t *testing.T) {
	main := mustRead(t, "../../cmd/manyforge/main.go")
	validate := strings.Index(main, "cfg.ValidateServe()")
	ensure := strings.Index(main, "cfg.EnsureReplyTokenKey()")
	firstUse := strings.Index(main, "cfg.InboundReplyTokenSecret")
	if validate < 0 || ensure < validate || firstUse < ensure {
		t.Error("main.go must call cfg.EnsureReplyTokenKey() after ValidateServe and before using the key")
	}
	if strings.Contains(main, "threading falls back to RFC822 headers; the webhook path") {
		t.Error("stale 'empty key falls back' comment is back in main.go")
	}
}
