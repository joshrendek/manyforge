package notify

import (
	"bytes"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"strings"
	"testing"
)

func TestBuildMIMEHasThreadingHeaders(t *testing.T) {
	m := Mail{
		From: "support@inbound.localhost", To: "ada@example.com", Subject: "Re: login broken",
		BodyText: "We are looking into it.", MessageID: "out-1@inbound.localhost",
		InReplyTo: "in-1@example.com", References: []string{"in-1@example.com"},
		ReplyTo: "support+TOKEN.SIG@inbound.localhost",
	}
	raw, err := buildMIME(m)
	if err != nil {
		t.Fatalf("buildMIME: %v", err)
	}
	s := string(raw)
	for _, want := range []string{
		"Message-ID: <out-1@inbound.localhost>", "In-Reply-To: <in-1@example.com>",
		"References: <in-1@example.com>", "Reply-To: support+TOKEN.SIG@inbound.localhost",
		"From: support@inbound.localhost", "To: ada@example.com", "Subject: Re: login broken",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("MIME missing %q\n---\n%s", want, s)
		}
	}
	if strings.Contains(s, "Auto-Submitted:") {
		t.Errorf("unexpected Auto-Submitted on a human reply")
	}
}

func TestBuildMIMEStampsAutoSubmittedWhenSet(t *testing.T) {
	raw, err := buildMIME(Mail{From: "a@b", To: "c@d", Subject: "s", MessageID: "m@b", AutoSubmitted: "auto-replied"})
	if err != nil {
		t.Fatalf("buildMIME: %v", err)
	}
	if !strings.Contains(string(raw), "Auto-Submitted: auto-replied") {
		t.Errorf("missing Auto-Submitted header")
	}
}

func TestBuildMIMERejectsHeaderInjection(t *testing.T) {
	_, err := buildMIME(Mail{From: "a@b", To: "c@d", MessageID: "m@b",
		Subject: "hi\r\nBcc: evil@example.com"})
	if err == nil {
		t.Fatalf("buildMIME accepted a Subject with CRLF (header injection)")
	}
}

// TestBuildMIMEMultipartAlternativeWhenHTML (manyforge-7c0) — when BodyHTML is set,
// the message is multipart/alternative carrying BOTH a text/plain and a text/html
// part (plain first, html last per RFC 2046 so clients render the richest they can).
// body_html is plumbed end-to-end but was previously dropped at this layer.
func TestBuildMIMEMultipartAlternativeWhenHTML(t *testing.T) {
	m := Mail{
		From: "support@inbound.localhost", To: "ada@example.com", Subject: "Re: hi",
		MessageID: "out-2@inbound.localhost",
		BodyText:  "plain version",
		BodyHTML:  "<p>html <b>version</b></p>",
	}
	raw, err := buildMIME(m)
	if err != nil {
		t.Fatalf("buildMIME: %v", err)
	}

	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("parse message: %v", err)
	}
	mediaType, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("parse Content-Type: %v", err)
	}
	if mediaType != "multipart/alternative" {
		t.Fatalf("Content-Type = %q, want multipart/alternative", mediaType)
	}
	boundary := params["boundary"]
	if boundary == "" {
		t.Fatalf("multipart message has no boundary")
	}

	type part struct{ ctype, body string }
	var parts []part
	mr := multipart.NewReader(msg.Body, boundary)
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("next part: %v", err)
		}
		body, _ := io.ReadAll(p)
		parts = append(parts, part{p.Header.Get("Content-Type"), strings.TrimSpace(string(body))})
	}
	if len(parts) != 2 {
		t.Fatalf("got %d parts, want 2 (text + html)", len(parts))
	}
	if !strings.HasPrefix(parts[0].ctype, "text/plain") || parts[0].body != "plain version" {
		t.Errorf("part[0] = %+v, want text/plain %q first", parts[0], "plain version")
	}
	if !strings.HasPrefix(parts[1].ctype, "text/html") || parts[1].body != "<p>html <b>version</b></p>" {
		t.Errorf("part[1] = %+v, want text/html %q last", parts[1], "<p>html <b>version</b></p>")
	}
}

// TestBuildMIMETextOnlyStaysSinglePart — the common case (no BodyHTML) stays a
// single-part text/plain message; the multipart path must not regress it.
func TestBuildMIMETextOnlyStaysSinglePart(t *testing.T) {
	raw, err := buildMIME(Mail{From: "a@b", To: "c@d", Subject: "s", MessageID: "m@b", BodyText: "hello"})
	if err != nil {
		t.Fatalf("buildMIME: %v", err)
	}
	s := string(raw)
	if !strings.Contains(s, "Content-Type: text/plain; charset=utf-8") {
		t.Errorf("text-only mail should be single-part text/plain:\n%s", s)
	}
	if strings.Contains(s, "multipart/alternative") {
		t.Errorf("text-only mail must NOT be multipart:\n%s", s)
	}
	if !strings.Contains(s, "hello") {
		t.Errorf("missing body text")
	}
}
