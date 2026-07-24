package feedback

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
)

// errNoSignature ⇒ the X-Feedback-Signature header was absent → treat the request as anonymous.
var errNoSignature = errors.New("feedback: no signature header")

// sigMaxSkew bounds replay: a signature timestamp farther than this from now is rejected.
const sigMaxSkew = 300 * time.Second

// feedbackSigningString is the exact byte string a verified caller signs:
// "<t>.<METHOD>.<path>.<body>". Binding method+path stops a captured MAC from being replayed
// against a different endpoint/post; the (signed) body carries any idempotency_key.
func feedbackSigningString(t int64, method, path string, body []byte) []byte {
	head := strconv.FormatInt(t, 10) + "." + method + "." + path + "."
	out := make([]byte, 0, len(head)+len(body))
	out = append(out, head...)
	out = append(out, body...)
	return out
}

// verifyFeedbackSignature validates the X-Feedback-Signature header against secret over the
// signing string. nil = verified; errNoSignature = absent (→ anon); any other error = present
// but invalid/expired/malformed (→ 401). Constant-time compare via hmac.Equal.
func verifyFeedbackSignature(header, secret, method, path string, body []byte, now time.Time) error {
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
		return errors.New("feedback: malformed signature header")
	}
	ts, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return errors.New("feedback: bad signature timestamp")
	}
	skew := now.Unix() - ts
	if skew < 0 {
		skew = -skew
	}
	if time.Duration(skew)*time.Second > sigMaxSkew {
		return errors.New("feedback: signature expired")
	}
	want, err := hex.DecodeString(v1)
	if err != nil {
		return errors.New("feedback: bad signature encoding")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(feedbackSigningString(ts, method, path, body))
	if !hmac.Equal(want, mac.Sum(nil)) {
		return errors.New("feedback: signature mismatch")
	}
	return nil
}
