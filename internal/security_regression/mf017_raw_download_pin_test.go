// Finding MF-017-RAW-DOWNLOAD (doc_support017.go / spec 017 R2, R6).
//
// The production netsafe wiring is pinned in resend_received_netsafe_pin_test.go.
package security_regression

import (
	"strings"
	"testing"
)

func TestPin_MF017RawDownloadBoundary(t *testing.T) {
	src := mustRead(t, "../mailing/provider/resend_receiving.go")
	body := mf017Slice(t, src, "func (r *Resend) DownloadReceivedRaw(", "func (r *Resend) EnsureReceivingEvents(")
	for _, pin := range []string{
		`u.Scheme != "https"`,                   // https-only (http only under the test opt-in)
		`req.URL.Scheme != "https"`,             // redirects stay https-only
		"len(via) > maxRawRedirects",            // and bounded
		"if !r.AllowInsecureRawURL {",           // no RawClient ⇒ fail closed in production
		"io.LimitReader(resp.Body, maxBytes+1)", // size cap
		"urlErr.Err",                            // transport errors drop the signed URL
	} {
		if !strings.Contains(body, pin) {
			t.Errorf("DownloadReceivedRaw missing %q", pin)
		}
	}
	if strings.Contains(body, "Authorization") || strings.Contains(body, "APIKey") {
		t.Error("the pre-signed raw URL must never receive the API key")
	}
}
