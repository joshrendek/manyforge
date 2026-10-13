package inbox

import (
	"testing"
)

// sesAuthResults is the real shape Resend's receiving pipeline (Amazon SES)
// prepends to inbound mail, verified against Resend-received messages.
const sesAuthResults = "Authentication-Results: amazonses.com;\r\n" +
	" spf=pass (spf: domain of example.com designates 192.0.2.1 as permitted sender) smtp.mailfrom=ada@example.com;\r\n" +
	" dkim=pass header.i=@example.com;\r\n" +
	" dmarc=none header.from=example.com;\r\n"

func authMessage(authHeaders string) []byte {
	return []byte("From: CEO <ceo@victim.example>\r\n" +
		"To: support@acme.test\r\n" +
		"Subject: wire transfer\r\n" +
		"Message-ID: <auth-1@victim.example>\r\n" +
		authHeaders +
		"\r\n" +
		"please pay\r\n")
}

var resendTrusted = []string{"amazonses.com"}

// SECREVIEW-M6: the header stamped by the trusted receiving hop is used.
func TestTrustedAuthResults_TrustedAuthservIDAccepted(t *testing.T) {
	pe, _ := Parse(authMessage(sesAuthResults))
	got := trustedAuthResults(pe.AuthenticationResults, resendTrusted)
	want := AuthResults{SPF: "pass", DKIM: "pass", DMARC: "none"}
	if got != want {
		t.Fatalf("auth = %+v, want %+v", got, want)
	}
	// authserv-id matching is case-insensitive (RFC 8601 ids are domain names).
	if got := trustedAuthResults(pe.AuthenticationResults, []string{"AmazonSES.com"}); got != want {
		t.Fatalf("case-insensitive match: auth = %+v, want %+v", got, want)
	}
}

// SECREVIEW-M6: a sender-supplied header with an untrusted authserv-id is
// ignored even when it is the FIRST Authentication-Results header; the first
// header with a trusted authserv-id is used instead.
func TestTrustedAuthResults_ForgedFirstHeaderIgnored(t *testing.T) {
	forged := "Authentication-Results: attacker.invalid; spf=pass; dkim=pass; dmarc=pass\r\n"
	sesFail := "Authentication-Results: amazonses.com; spf=fail smtp.mailfrom=ceo@victim.example;" +
		" dkim=none; dmarc=fail header.from=victim.example\r\n"
	pe, _ := Parse(authMessage(forged + sesFail))
	if len(pe.AuthenticationResults) != 2 {
		t.Fatalf("parsed %d Authentication-Results headers, want 2 (in order)", len(pe.AuthenticationResults))
	}
	got := trustedAuthResults(pe.AuthenticationResults, resendTrusted)
	want := AuthResults{SPF: "fail", DKIM: "none", DMARC: "fail"}
	if got != want {
		t.Fatalf("auth = %+v, want the trusted header's verdicts %+v", got, want)
	}
}

// SECREVIEW-M6: SES prepends its header, so a sender-forged header that reuses
// the trusted authserv-id sits BELOW the real one and must not win.
func TestTrustedAuthResults_FirstTrustedHeaderWins(t *testing.T) {
	forgedSES := "Authentication-Results: amazonses.com; spf=pass; dkim=pass; dmarc=pass\r\n"
	pe, _ := Parse(authMessage("Authentication-Results: amazonses.com; spf=fail; dkim=fail; dmarc=fail\r\n" + forgedSES))
	got := trustedAuthResults(pe.AuthenticationResults, resendTrusted)
	if want := (AuthResults{SPF: "fail", DKIM: "fail", DMARC: "fail"}); got != want {
		t.Fatalf("auth = %+v, want the topmost trusted header %+v", got, want)
	}
}

// SECREVIEW-M6: with no trusted header (or no trusted ids configured) every
// verdict is unknown (empty, projected as "unknown"), never the sender's claim.
func TestTrustedAuthResults_NoneTrustedIsUnknown(t *testing.T) {
	forged := "Authentication-Results: attacker.invalid; spf=pass; dkim=pass; dmarc=pass\r\n"
	pe, _ := Parse(authMessage(forged))
	if got := trustedAuthResults(pe.AuthenticationResults, resendTrusted); got != (AuthResults{}) {
		t.Fatalf("untrusted only: auth = %+v, want unknown", got)
	}
	pe, _ = Parse(authMessage(sesAuthResults))
	if got := trustedAuthResults(pe.AuthenticationResults, nil); got != (AuthResults{}) {
		t.Fatalf("nothing trusted: auth = %+v, want unknown", got)
	}
	// A look-alike id (suffix / prefix of a trusted one) is not trusted.
	for _, h := range []string{
		"Authentication-Results: evil-amazonses.com; spf=pass; dkim=pass; dmarc=pass\r\n",
		"Authentication-Results: amazonses.com.evil.test; spf=pass; dkim=pass; dmarc=pass\r\n",
		"Authentication-Results: spf=pass; dkim=pass; dmarc=pass\r\n",
	} {
		pe, _ = Parse(authMessage(h))
		if got := trustedAuthResults(pe.AuthenticationResults, resendTrusted); got != (AuthResults{}) {
			t.Fatalf("%q: auth = %+v, want unknown", h, got)
		}
	}
}

// SECREVIEW-M6: the configured trust applies only to Resend-received mail. On
// the SMTP receiver and the JSON webhook a sender-written "amazonses.com" header
// is the topmost one, so it must not be believed.
func TestTrustedAuthservForScopesTrustToResend(t *testing.T) {
	s := &Service{cfg: Config{TrustedAuthservIDs: []string{"amazonses.com"}}}
	forged := []string{"amazonses.com; spf=pass smtp.mailfrom=ceo@victim.example; dkim=pass header.i=@victim.example; dmarc=pass header.from=victim.example"}

	got := trustedAuthResults(forged, s.trustedAuthservFor(resendSource))
	if got.SPF != "pass" || got.DKIM != "pass" || got.DMARC != "pass" {
		t.Fatalf("resend path: want the SES verdicts, got %+v", got)
	}
	for _, provider := range []string{"smtp", "webhook:postmark", ""} {
		got := trustedAuthResults(forged, s.trustedAuthservFor(provider))
		if got != (AuthResults{}) && (got.SPF != "unknown" || got.DKIM != "unknown" || got.DMARC != "unknown") {
			t.Errorf("provider %q: forged header must not be trusted, got %+v", provider, got)
		}
	}
}
