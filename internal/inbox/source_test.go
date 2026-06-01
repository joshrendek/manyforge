package inbox

import (
	"strings"
	"testing"
)

// wellFormed is a multipart/mixed message carrying a text body, an HTML body,
// a PNG attachment, threading headers (Message-ID/In-Reply-To/References), and
// an Authentication-Results header. It exercises every extraction path at once.
const wellFormed = "From: Ada Lovelace <ada@example.com>\r\n" +
	"To: support@acme.test, ops@acme.test\r\n" +
	"Cc: cc@acme.test\r\n" +
	"Subject: Re: my order is late\r\n" +
	"Date: Mon, 02 Jan 2006 15:04:05 -0700\r\n" +
	"Message-ID: <msg-2@example.com>\r\n" +
	"In-Reply-To: <msg-1@acme.test>\r\n" +
	"References: <msg-0@acme.test> <msg-1@acme.test>\r\n" +
	"Authentication-Results: mx.acme.test; spf=pass smtp.mailfrom=ada@example.com; " +
	"dkim=pass header.d=example.com; dmarc=fail header.from=example.com\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=BOUND\r\n" +
	"\r\n" +
	"--BOUND\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"plain body here\r\n" +
	"--BOUND\r\n" +
	"Content-Type: text/html; charset=utf-8\r\n" +
	"\r\n" +
	"<p>html body here</p>\r\n" +
	"--BOUND\r\n" +
	"Content-Type: image/png; name=\"pic.png\"\r\n" +
	"Content-Disposition: attachment; filename=\"pic.png\"\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"\r\n" +
	"iVBORw0KGgo=\r\n" +
	"--BOUND--\r\n"

// plainNoMessageID is a bare text message with NO Message-ID header. We must not
// synthesize one (that is T025's job); MessageID stays empty.
const plainNoMessageID = "From: Grace <grace@example.com>\r\n" +
	"To: support@acme.test\r\n" +
	"Subject: hello with no id\r\n" +
	"\r\n" +
	"just a plain body\r\n"

// autoReplied carries Auto-Submitted: auto-replied — the FR-018 loop-guard signal.
const autoReplied = "From: bot@example.com\r\n" +
	"To: support@acme.test\r\n" +
	"Subject: Out of office\r\n" +
	"Message-ID: <ooo@example.com>\r\n" +
	"Auto-Submitted: auto-replied\r\n" +
	"\r\n" +
	"I am away.\r\n"

func TestParse_WellFormed(t *testing.T) {
	pe, err := Parse([]byte(wellFormed))
	if err != nil {
		t.Fatalf("Parse returned error on well-formed mail: %v", err)
	}
	if pe == nil {
		t.Fatal("Parse returned nil *ParsedEmail")
	}
	if pe.From.Address != "ada@example.com" {
		t.Errorf("From.Address = %q, want ada@example.com", pe.From.Address)
	}
	if pe.From.Name != "Ada Lovelace" {
		t.Errorf("From.Name = %q, want Ada Lovelace", pe.From.Name)
	}
	// Recipients are To + Cc.
	wantRcpt := map[string]bool{"support@acme.test": true, "ops@acme.test": true, "cc@acme.test": true}
	if len(pe.Recipients) != len(wantRcpt) {
		t.Errorf("Recipients = %v, want %d entries", pe.Recipients, len(wantRcpt))
	}
	for _, r := range pe.Recipients {
		if !wantRcpt[r] {
			t.Errorf("unexpected recipient %q", r)
		}
	}
	if pe.Subject != "Re: my order is late" {
		t.Errorf("Subject = %q", pe.Subject)
	}
	if pe.MessageID != "msg-2@example.com" {
		t.Errorf("MessageID = %q, want msg-2@example.com (angle brackets stripped)", pe.MessageID)
	}
	if pe.InReplyTo != "msg-1@acme.test" {
		t.Errorf("InReplyTo = %q, want msg-1@acme.test", pe.InReplyTo)
	}
	wantRefs := []string{"msg-0@acme.test", "msg-1@acme.test"}
	if len(pe.References) != len(wantRefs) {
		t.Fatalf("References = %v, want %v", pe.References, wantRefs)
	}
	for i, ref := range wantRefs {
		if pe.References[i] != ref {
			t.Errorf("References[%d] = %q, want %q", i, pe.References[i], ref)
		}
	}
	if pe.Date.IsZero() {
		t.Error("Date is zero, want parsed Date header")
	}
	if !strings.Contains(pe.TextBody, "plain body here") {
		t.Errorf("TextBody = %q, want it to contain the plain body", pe.TextBody)
	}
	if !strings.Contains(pe.HTMLBody, "html body here") {
		t.Errorf("HTMLBody = %q, want it to contain the html body", pe.HTMLBody)
	}
	if len(pe.Attachments) != 1 {
		t.Fatalf("Attachments = %d, want 1", len(pe.Attachments))
	}
	att := pe.Attachments[0]
	if att.FileName != "pic.png" {
		t.Errorf("attachment FileName = %q, want pic.png", att.FileName)
	}
	if att.DeclaredContentType != "image/png" {
		t.Errorf("attachment DeclaredContentType = %q, want image/png", att.DeclaredContentType)
	}
	if len(att.Content) == 0 {
		t.Error("attachment Content is empty, want decoded bytes")
	}
	// Authentication-Results parsed into typed fields (FR-019).
	if pe.Auth.SPF != "pass" {
		t.Errorf("Auth.SPF = %q, want pass", pe.Auth.SPF)
	}
	if pe.Auth.DKIM != "pass" {
		t.Errorf("Auth.DKIM = %q, want pass", pe.Auth.DKIM)
	}
	if pe.Auth.DMARC != "fail" {
		t.Errorf("Auth.DMARC = %q, want fail", pe.Auth.DMARC)
	}
	// Not an auto-reply.
	if pe.Auto.IsAutoReply {
		t.Error("Auto.IsAutoReply = true, want false for a normal human reply")
	}
}

func TestParse_PlainNoMessageID(t *testing.T) {
	pe, err := Parse([]byte(plainNoMessageID))
	if err != nil {
		t.Fatalf("Parse returned error on plain mail: %v", err)
	}
	if pe == nil {
		t.Fatal("Parse returned nil *ParsedEmail")
	}
	if pe.MessageID != "" {
		t.Errorf("MessageID = %q, want empty (no synthesis here — T025 owns that)", pe.MessageID)
	}
	if pe.From.Address != "grace@example.com" {
		t.Errorf("From.Address = %q", pe.From.Address)
	}
	if !strings.Contains(pe.TextBody, "just a plain body") {
		t.Errorf("TextBody = %q", pe.TextBody)
	}
	if len(pe.Attachments) != 0 {
		t.Errorf("Attachments = %d, want 0", len(pe.Attachments))
	}
	if pe.InReplyTo != "" {
		t.Errorf("InReplyTo = %q, want empty", pe.InReplyTo)
	}
	if len(pe.References) != 0 {
		t.Errorf("References = %v, want empty", pe.References)
	}
}

func TestParse_MalformedDegrades(t *testing.T) {
	cases := map[string][]byte{
		"garbage":      []byte("\x00\x01\x02 not an email at all \xff\xfe"),
		"empty":        {},
		"headers only": []byte("This is just a line with no structure\r\n"),
		"truncated multipart": []byte("Content-Type: multipart/mixed; boundary=X\r\n\r\n--X\r\n" +
			"Content-Type: text/plain\r\n\r\nbody but no closing boundary"),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			// Must not panic on garbage.
			pe, err := Parse(raw)
			_ = err // a wrapped error is acceptable; a non-nil result is required.
			if pe == nil {
				t.Fatal("Parse returned nil *ParsedEmail on malformed input; must degrade to a non-nil best-effort result")
			}
		})
	}
}

func TestParse_AutoReplyHeaders(t *testing.T) {
	pe, err := Parse([]byte(autoReplied))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if !pe.Auto.IsAutoReply {
		t.Error("Auto.IsAutoReply = false, want true for Auto-Submitted: auto-replied (FR-018)")
	}
}

func TestParse_AutoReplySignals(t *testing.T) {
	cases := []struct {
		name   string
		header string
		want   bool
	}{
		{"auto-submitted auto-replied", "Auto-Submitted: auto-replied\r\n", true},
		{"auto-submitted auto-generated", "Auto-Submitted: auto-generated\r\n", true},
		{"auto-submitted no is NOT auto", "Auto-Submitted: no\r\n", false},
		{"precedence bulk", "Precedence: bulk\r\n", true},
		{"precedence list", "Precedence: list\r\n", true},
		{"precedence junk", "Precedence: junk\r\n", true},
		{"x-auto-response-suppress", "X-Auto-Response-Suppress: OOF\r\n", true},
		{"list-id present", "List-Id: <list.acme.test>\r\n", true},
		{"none of them", "X-Mailer: handwritten\r\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := "From: x@example.com\r\nTo: support@acme.test\r\nSubject: s\r\n" +
				tc.header + "\r\nbody\r\n"
			pe, err := Parse([]byte(raw))
			if err != nil {
				t.Fatalf("Parse error: %v", err)
			}
			if pe.Auto.IsAutoReply != tc.want {
				t.Errorf("IsAutoReply = %v, want %v for header %q", pe.Auto.IsAutoReply, tc.want, tc.header)
			}
		})
	}
}

func TestParse_AuthResultsAbsent(t *testing.T) {
	pe, err := Parse([]byte(plainNoMessageID))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if pe.Auth.SPF != "" || pe.Auth.DKIM != "" || pe.Auth.DMARC != "" {
		t.Errorf("Auth = %+v, want all empty when Authentication-Results absent", pe.Auth)
	}
}
