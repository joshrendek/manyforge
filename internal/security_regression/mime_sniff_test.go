//go:build integration

// Finding MF-002-MIME-SNIFF (doc_support.go / SC-007 / FR-007): an attachment's
// DECLARED Content-Type is never trusted. A part that declares image/png but
// whose bytes sniff to a disallowed type (an ELF executable, outside the
// allowlist) is rejected BEFORE any row is persisted — no ticket, no message, no
// attachment is written.

package security_regression

import (
	"context"
	"encoding/base64"
	"testing"
	"time"

	"github.com/manyforge/manyforge/internal/inbox"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

// elfBytes is a minimal ELF header (\x7fELF…). http.DetectContentType sniffs this
// as application/octet-stream — NOT in blob.Sniff's allowlist — regardless of the
// part's declared Content-Type.
var elfBytes = []byte{
	0x7f, 'E', 'L', 'F', 0x02, 0x01, 0x01, 0x00,
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	0x02, 0x00, 0x3e, 0x00, 0x01, 0x00, 0x00, 0x00,
}

// pngBytes is a minimal valid PNG (signature + IHDR start). http.DetectContentType
// sniffs this as image/png — which IS in blob.Sniff's allowlist — so a clean
// attachment is stored. Used as the positive control.
var pngBytes = []byte{
	0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a,
	0x00, 0x00, 0x00, 0x0d, 'I', 'H', 'D', 'R',
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4, 0x89,
}

// attachmentMessage builds a multipart/mixed message: a text part plus one
// attachment that DECLARES image/png but actually carries the given bytes.
func attachmentMessage(to, from, subject, messageID string, attachment []byte) []byte {
	b64 := base64.StdEncoding.EncodeToString(attachment)
	return []byte("From: " + from + "\r\n" +
		"To: " + to + "\r\n" +
		"Subject: " + subject + "\r\n" +
		"Date: Mon, 02 Jan 2006 15:04:05 -0700\r\n" +
		"Message-ID: <" + messageID + ">\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=BOUND\r\n" +
		"\r\n" +
		"--BOUND\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"please see attached\r\n" +
		"--BOUND\r\n" +
		"Content-Type: image/png; name=\"file.png\"\r\n" +
		"Content-Disposition: attachment; filename=\"file.png\"\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"\r\n" +
		b64 + "\r\n" +
		"--BOUND--\r\n")
}

func ingestAttachment(ctx context.Context, ten supportTenant, from, subject, messageID string, attachment []byte) inbox.RawMessage {
	return inbox.RawMessage{
		Provider:          "webhook:test",
		EnvelopeRecipient: ten.address,
		EnvelopeSender:    from,
		ReceivedAt:        time.Now(),
		Raw:               attachmentMessage(ten.address, from, subject, messageID, attachment),
	}
}

// TestLyingAttachmentRejectedBeforePersistence (MF-002-MIME-SNIFF / SC-007).
//
//   - The lying attachment (declared image/png, actually an ELF executable) is
//     dropped: ZERO attachment rows are ever written for it — the sniff happens
//     before persistence.
//   - Positive control: a message carrying a *genuine* PNG (allowlisted) DOES
//     persist a ticket_message and exactly one attachment row — proving the path
//     isn't simply rejecting all attachments. The control fails until GREEN, which
//     keeps this a meaningful RED baseline (a no-op stub cannot satisfy it).
func TestLyingAttachmentRejectedBeforePersistence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	ten := seedSupportTenant(ctx, t, tdb)
	svc := newSupportIngest(ctx, t, tdb)

	// Lying attachment: ingest may error or drop the part; either way nothing of
	// it is persisted as an attachment.
	_, _ = svc.Ingest(ctx, ingestAttachment(ctx, ten, "Mallory <mallory@example.com>", "gift", "mime-bad@example.com", elfBytes))
	if n := countSuperInt(ctx, t, tdb.Super, "SELECT count(*) FROM attachment WHERE business_id=$1", ten.business); n != 0 {
		t.Errorf("attachment count after lying attachment = %d, want 0 — a disallowed/lying type must never be persisted", n)
	}

	// Positive control: a genuine PNG is allowlisted and stored. This proves the
	// rejection above is type-driven (not blanket-drop) and is the RED control:
	// it cannot pass while Ingest is a no-op stub.
	resOK, err := svc.Ingest(ctx, ingestAttachment(ctx, ten, "Grace <grace@example.com>", "real png", "mime-ok@example.com", pngBytes))
	if err != nil {
		t.Fatalf("control: ingest genuine PNG: %v", err)
	}
	if n := countSuperInt(ctx, t, tdb.Super, "SELECT count(*) FROM ticket_message WHERE ticket_id=$1", resOK.TicketID); n != 1 {
		t.Errorf("control: ticket_message count = %d, want 1 (a clean message must persist)", n)
	}
	if n := countSuperInt(ctx, t, tdb.Super, "SELECT count(*) FROM attachment WHERE business_id=$1", ten.business); n != 1 {
		t.Errorf("control: attachment count = %d, want 1 (a genuine allowlisted PNG must be stored)", n)
	}
}
