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
// "<t>.<METHOD>.<target>.<body>". Binding method+target(+query)+body stops a captured MAC from
// being replayed against a different endpoint, a different post, OR a different query (e.g. a
// signed GET list replayed with a different voter_identity/author) — the (signed) body also
// carries any idempotency_key.
//
// <target> is the FULL request-target the server verifies against — path + "?" + raw query
// string, i.e. exactly what r.URL.RequestURI() returns. It INCLUDES the /api/v1 prefix, e.g.
// "/api/v1/feedback/public/fbk_ABC.../posts?voter_identity=alice" — a caller that signs the bare
// "/feedback/public/..." path, or that omits/reorders query params from what it actually sends,
// computes a wrong MAC and is rejected 401.
//
// feedbackSigningString frames the signed bytes as "<t>.<METHOD>.<target>.<body>". The unescaped
// "." delimiter between target and body is unambiguous for the routed path + key/id segments (no
// "." in HTTP method verbs or fbk_/UUID identifiers) and a JSON body (never starts with a bare
// "."). Caller-supplied query VALUES (e.g. voter_identity, author) MAY contain ".": this does not
// open a target/body boundary-shift because every GET this scheme signs verifies against a fixed
// empty body (see resolveVerified's GET call site) — there is no non-empty body for a shifted
// split to land on. Do NOT reuse this framing for a body-bearing GET, or for methods whose target
// can otherwise vary independently of the body, without switching to length-prefixed framing.
func feedbackSigningString(t int64, method, target string, body []byte) []byte {
	head := strconv.FormatInt(t, 10) + "." + method + "." + target + "."
	out := make([]byte, 0, len(head)+len(body))
	out = append(out, head...)
	out = append(out, body...)
	return out
}

// verifyFeedbackSignature validates the X-Feedback-Signature header against secret over the
// signing string. nil = verified; errNoSignature = absent (→ anon); any other error = present
// but invalid/expired/malformed (→ 401). Constant-time compare via hmac.Equal.
func verifyFeedbackSignature(header, secret, method, target string, body []byte, now time.Time) error {
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
	maxSkewSec := int64(sigMaxSkew / time.Second) // 300
	nowUnix := now.Unix()
	if ts < nowUnix-maxSkewSec || ts > nowUnix+maxSkewSec {
		return errors.New("feedback: signature expired")
	}
	want, err := hex.DecodeString(v1)
	if err != nil {
		return errors.New("feedback: bad signature encoding")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(feedbackSigningString(ts, method, target, body))
	if !hmac.Equal(want, mac.Sum(nil)) {
		return errors.New("feedback: signature mismatch")
	}
	return nil
}
