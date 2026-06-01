package notify

import (
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
