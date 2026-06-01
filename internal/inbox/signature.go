package inbox

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"strings"
)

// verifyHMAC is the SINGLE source of truth for inbound-webhook signature
// verification, shared by every public (no-JWT) ingress in this package — the
// inbound email webhook (WebhookAdapter.verify) and the hard-bounce webhook
// (BounceHandler.verify). Keeping ONE crypto body means a future change to the
// scheme (e.g. a replay timestamp-skew window, or hex/case normalization) cannot
// silently reach one transport and not the other, leaving their auth strength
// divergent.
//
// It checks an X-MF-Signature against an HMAC-SHA256 over the signed content in
// constant time. When timestamp is non-empty it is bound into the signed content
// ("<timestamp>.<body>") for replay defense; otherwise the body alone is signed. The
// provided signature is accepted as bare lowercase hex or with a "sha256=" prefix
// (some providers prefix it). An empty secret OR empty signature rejects everything
// (fail closed). Returns true ONLY on a valid signature. The constant-time compare
// ensures a forged signature cannot be brute-forced byte by byte via response timing.
func verifyHMAC(secret []byte, sig, timestamp string, body []byte) bool {
	if len(secret) == 0 || sig == "" {
		return false
	}
	sig = strings.TrimPrefix(strings.TrimSpace(sig), "sha256=")
	provided, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	if timestamp != "" {
		mac.Write([]byte(timestamp))
		mac.Write([]byte("."))
	}
	mac.Write(body)
	expected := mac.Sum(nil)
	return subtle.ConstantTimeCompare(provided, expected) == 1
}
