package telemetry

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
)

// errNoSignature ⇒ the X-Telemetry-Signature header was absent.
var errNoSignature = errors.New("telemetry: no signature header")

// sigMaxSkew bounds replay: a signature timestamp farther than this from now is rejected.
const sigMaxSkew = 300 * time.Second

// telemetrySigningString is the exact byte string a signing caller MACs:
// "<t>.<METHOD>.<target>.<body>". Binding method + full request-target + body stops a captured
// MAC from being replayed against a different endpoint, a different client key, or a different
// payload.
//
// <target> is the FULL request-target including the /api/v1 prefix and any query string — exactly
// what r.URL.RequestURI() returns. Signing the bare "/telemetry/ingest/..." path produces a wrong
// MAC and is rejected.
//
// Ingest is always a POST carrying a JSON body, so the unescaped "." delimiter is unambiguous
// here: no HTTP verb or mfk_ key contains ".", and a JSON body never starts with a bare ".".
func telemetrySigningString(t int64, method, target string, body []byte) []byte {
	head := strconv.FormatInt(t, 10) + "." + method + "." + target + "."
	out := make([]byte, 0, len(head)+len(body))
	out = append(out, head...)
	out = append(out, body...)
	return out
}

// verifyTelemetrySignature validates the X-Telemetry-Signature header against secret over the
// signing string. nil = verified; errNoSignature = absent; any other error = present but
// invalid/expired/malformed. hmac.Equal is a constant-time compare.
func verifyTelemetrySignature(header, secret, method, target string, body []byte, now time.Time) error {
	if strings.TrimSpace(header) == "" {
		return errNoSignature
	}
	var tsStr, v1 string
	for _, part := range strings.Split(header, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "t":
			tsStr = kv[1]
		case "v1":
			v1 = kv[1]
		}
	}
	if tsStr == "" || v1 == "" {
		return errors.New("telemetry: malformed signature header")
	}
	ts, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return errors.New("telemetry: bad signature timestamp")
	}
	maxSkewSec := int64(sigMaxSkew / time.Second)
	nowUnix := now.Unix()
	if ts < nowUnix-maxSkewSec || ts > nowUnix+maxSkewSec {
		return errors.New("telemetry: signature expired")
	}
	want, err := hex.DecodeString(v1)
	if err != nil {
		return errors.New("telemetry: bad signature encoding")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(telemetrySigningString(ts, method, target, body))
	if !hmac.Equal(want, mac.Sum(nil)) {
		return errors.New("telemetry: signature mismatch")
	}
	return nil
}
