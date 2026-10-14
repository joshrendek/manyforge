//go:build integration

package inbox

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/ticketing"
)

var dropELF = []byte{
	0x7f, 'E', 'L', 'F', 0x02, 0x01, 0x01, 0x00,
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	0x02, 0x00, 0x3e, 0x00, 0x01, 0x00, 0x00, 0x00,
}

// mixedAttachmentMessage: a text part, an allowlisted PNG, and an ELF executable
// that declares itself application/pdf with the given (RFC 2231-encoded) filename.
func mixedAttachmentMessage(to, from, subject, messageID, encodedFilename string) []byte {
	return []byte("From: " + from + "\r\n" +
		"To: " + to + "\r\n" +
		"Subject: " + subject + "\r\n" +
		"Date: Mon, 02 Jan 2006 15:04:05 -0700\r\n" +
		"Message-ID: <" + messageID + ">\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=BOUND\r\n\r\n" +
		"--BOUND\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nsee attached\r\n" +
		"--BOUND\r\nContent-Type: image/png; name=\"ok.png\"\r\n" +
		"Content-Disposition: attachment; filename=\"ok.png\"\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
		base64.StdEncoding.EncodeToString(replayPNG) + "\r\n" +
		"--BOUND\r\nContent-Type: application/pdf\r\n" +
		"Content-Disposition: attachment; filename*=" + encodedFilename + "\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
		base64.StdEncoding.EncodeToString(dropELF) + "\r\n" +
		"--BOUND--\r\n")
}

// Spec 017 R15 / MF-002-MIME-SNIFF: a disallowed attachment no longer fails the
// message (which made the sender retry until it bounced); it is dropped and
// recorded, and the allowlisted attachment is still stored.
func TestIngestDropsDisallowedAttachmentAndKeepsMessage(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	ten := seedIngestTenant(ctx, t, tdb)
	svc := newIngestService(ctx, t, tdb)

	msg := RawMessage{
		Provider: "webhook:test", EnvelopeRecipient: ten.address, EnvelopeSender: "mallory@example.com",
		ReceivedAt: time.Now(),
		Raw:        mixedAttachmentMessage(ten.address, "Mallory <mallory@example.com>", "invoice", "drop-1@example.com", "utf-8''invoice.exe"),
	}
	res, err := svc.Ingest(ctx, msg)
	if err != nil {
		t.Fatalf("ingest with a disallowed attachment must succeed: %v", err)
	}
	if !res.Created {
		t.Fatalf("result = %+v, want a new ticket", res)
	}
	if n := countSuper(ctx, t, tdb.Super, "SELECT count(*) FROM attachment WHERE ticket_message_id=$1", res.MessageID); n != 1 {
		t.Fatalf("stored attachments = %d, want 1 (the PNG)", n)
	}
	var raw []byte
	if err := tdb.Super.QueryRow(ctx, "SELECT dropped_attachments FROM ticket_message WHERE id=$1", res.MessageID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var dropped []ticketing.DroppedAttachment
	if err := json.Unmarshal(raw, &dropped); err != nil {
		t.Fatal(err)
	}
	want := ticketing.DroppedAttachment{Filename: "invoice.exe", DetectedType: "application/octet-stream", Size: int64(len(dropELF)), Reason: ticketing.DropReasonDisallowedType}
	if len(dropped) != 1 || dropped[0] != want {
		t.Fatalf("dropped = %+v, want [%+v]", dropped, want)
	}

	again, err := svc.Ingest(ctx, msg)
	if err != nil || !again.Duplicate {
		t.Fatalf("replay = %+v, %v; want an idempotent duplicate", again, err)
	}
	if n := countSuper(ctx, t, tdb.Super, "SELECT count(*) FROM ticket_message WHERE ticket_id=$1", res.TicketID); n != 1 {
		t.Fatalf("messages after replay = %d, want 1", n)
	}
	var rawAfter []byte
	if err := tdb.Super.QueryRow(ctx, "SELECT dropped_attachments FROM ticket_message WHERE id=$1", res.MessageID).Scan(&rawAfter); err != nil {
		t.Fatal(err)
	}
	var droppedAfter []ticketing.DroppedAttachment
	if err := json.Unmarshal(rawAfter, &droppedAfter); err != nil {
		t.Fatal(err)
	}
	if len(droppedAfter) != 1 || droppedAfter[0] != want {
		t.Fatalf("dropped after replay = %+v, want [%+v] unchanged", droppedAfter, want)
	}
}

// A NUL or control byte in an attachment filename must not fail the ingest
// (jsonb rejects \u0000); it is stripped before recording.
func TestIngestDroppedAttachmentFilenameWithNULStillIngests(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	ten := seedIngestTenant(ctx, t, tdb)
	svc := newIngestService(ctx, t, tdb)

	res, err := svc.Ingest(ctx, RawMessage{
		Provider: "webhook:test", EnvelopeRecipient: ten.address, EnvelopeSender: "mallory@example.com",
		ReceivedAt: time.Now(),
		Raw:        mixedAttachmentMessage(ten.address, "mallory@example.com", "nul", "drop-nul@example.com", "utf-8''bad%00na%07me.exe"),
	})
	if err != nil {
		t.Fatalf("ingest with a NUL filename must succeed: %v", err)
	}
	var raw []byte
	if err := tdb.Super.QueryRow(ctx, "SELECT dropped_attachments FROM ticket_message WHERE id=$1", res.MessageID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var dropped []ticketing.DroppedAttachment
	if err := json.Unmarshal(raw, &dropped); err != nil || len(dropped) != 1 || dropped[0].Filename != "badname.exe" {
		t.Fatalf("dropped = %+v (%v), want filename badname.exe", dropped, err)
	}
}

// I2a: with attachments disabled (no blob store) the allowlisted PNG is recorded
// as dropped alongside the disallowed part, so the thread shows both as not stored.
func TestIngestWithoutBlobStoreRecordsAllowedAttachmentAsDropped(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	ten := seedIngestTenant(ctx, t, tdb)
	svc := NewService(tdb.App, nil, Config{
		ReplyTokenKey:       []byte("test-reply-token-key-0123456789ab"),
		AttachmentMaxBytes:  25 << 20,
		InboundSystemDomain: systemDomain,
	}, slog.New(slog.NewTextHandler(nopWriter{}, nil)))

	res, err := svc.Ingest(ctx, RawMessage{
		Provider: "webhook:test", EnvelopeRecipient: ten.address, EnvelopeSender: "ada@example.com",
		ReceivedAt: time.Now(),
		Raw:        mixedAttachmentMessage(ten.address, "ada@example.com", "screenshot", "noblob-1@example.com", "utf-8''invoice.exe"),
	})
	if err != nil {
		t.Fatalf("ingest without a blob store: %v", err)
	}
	if n := countSuper(ctx, t, tdb.Super, "SELECT count(*) FROM attachment WHERE ticket_message_id=$1", res.MessageID); n != 0 {
		t.Fatalf("stored attachments = %d, want 0 without a blob store", n)
	}
	var raw []byte
	if err := tdb.Super.QueryRow(ctx, "SELECT dropped_attachments FROM ticket_message WHERE id=$1", res.MessageID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var dropped []ticketing.DroppedAttachment
	if err := json.Unmarshal(raw, &dropped); err != nil {
		t.Fatal(err)
	}
	want := []ticketing.DroppedAttachment{
		{Filename: "ok.png", DetectedType: "image/png", Size: int64(len(replayPNG)), Reason: ticketing.DropReasonStorageDisabled},
		{Filename: "invoice.exe", DetectedType: "application/octet-stream", Size: int64(len(dropELF)), Reason: ticketing.DropReasonDisallowedType},
	}
	if len(dropped) != len(want) || dropped[0] != want[0] || dropped[1] != want[1] {
		t.Fatalf("dropped = %+v, want %+v", dropped, want)
	}
}

// pngOnlyMessage carries one allowlisted PNG whose filename parameter is given
// verbatim (e.g. an RFC 2231 extended value).
func pngOnlyMessage(to, from, messageID, filenameParam string) []byte {
	return []byte("From: " + from + "\r\n" +
		"To: " + to + "\r\n" +
		"Subject: allowed png\r\n" +
		"Date: Mon, 02 Jan 2006 15:04:05 -0700\r\n" +
		"Message-ID: <" + messageID + ">\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=BOUND\r\n\r\n" +
		"--BOUND\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nsee attached\r\n" +
		"--BOUND\r\nContent-Type: image/png\r\n" +
		"Content-Disposition: attachment; " + filenameParam + "\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
		base64.StdEncoding.EncodeToString(replayPNG) + "\r\n" +
		"--BOUND--\r\n")
}

// SECREVIEW-M3: an ALLOWED attachment whose RFC 2231 filename decodes to a NUL
// must still ingest and be stored (with the control byte stripped). Before the
// fix the raw name reached the jsonb attachment metadata, Postgres rejected
// \u0000, and the webhook 503'd into a permanent provider retry loop.
func TestIngestStoredAttachmentFilenameWithNULStillIngests(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	ten := seedIngestTenant(ctx, t, tdb)
	svc := newIngestService(ctx, t, tdb)

	res, err := svc.Ingest(ctx, RawMessage{
		Provider: "webhook:test", EnvelopeRecipient: ten.address, EnvelopeSender: "mallory@example.com",
		ReceivedAt: time.Now(),
		Raw:        pngOnlyMessage(ten.address, "mallory@example.com", "stored-nul@example.com", "filename*=UTF-8''bad%00name.png"),
	})
	if err != nil {
		t.Fatalf("ingest of an allowed attachment with a NUL filename must succeed: %v", err)
	}
	var filename string
	if err := tdb.Super.QueryRow(ctx, "SELECT filename FROM attachment WHERE ticket_message_id=$1", res.MessageID).Scan(&filename); err != nil {
		t.Fatalf("stored attachment: %v", err)
	}
	if filename != "badname.png" {
		t.Fatalf("stored filename = %q, want badname.png", filename)
	}
}

// manyforge-22zq: a part dropped by the SECREVIEW-M5 accepted-count cap is
// recorded with reason limit_exceeded (not indistinguishable from a disallowed
// type), and the parts under the cap are stored.
func TestIngestRecordsOverCapAttachmentAsLimitExceeded(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	ten := seedIngestTenant(ctx, t, tdb)
	svc := newIngestService(ctx, t, tdb)

	var b strings.Builder
	b.WriteString("From: ada@example.com\r\nTo: " + ten.address + "\r\nSubject: many\r\n" +
		"Date: Mon, 02 Jan 2006 15:04:05 -0700\r\nMessage-ID: <cap-1@example.com>\r\n" +
		"MIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=BOUND\r\n\r\n" +
		"--BOUND\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nlots attached\r\n")
	enc := base64.StdEncoding.EncodeToString(replayPNG)
	for i := 0; i <= maxAcceptedAttachments; i++ {
		b.WriteString("--BOUND\r\nContent-Type: image/png\r\nContent-Disposition: attachment; filename=\"" +
			fmt.Sprintf("p%02d.png", i) + "\"\r\nContent-Transfer-Encoding: base64\r\n\r\n" + enc + "\r\n")
	}
	b.WriteString("--BOUND--\r\n")

	res, err := svc.Ingest(ctx, RawMessage{
		Provider: "webhook:test", EnvelopeRecipient: ten.address, EnvelopeSender: "ada@example.com",
		ReceivedAt: time.Now(), Raw: []byte(b.String()),
	})
	if err != nil {
		t.Fatalf("ingest over the attachment cap must succeed: %v", err)
	}
	if n := countSuper(ctx, t, tdb.Super, "SELECT count(*) FROM attachment WHERE ticket_message_id=$1", res.MessageID); n != maxAcceptedAttachments {
		t.Fatalf("stored attachments = %d, want the cap %d", n, maxAcceptedAttachments)
	}
	var raw []byte
	if err := tdb.Super.QueryRow(ctx, "SELECT dropped_attachments FROM ticket_message WHERE id=$1", res.MessageID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var dropped []ticketing.DroppedAttachment
	if err := json.Unmarshal(raw, &dropped); err != nil {
		t.Fatal(err)
	}
	want := ticketing.DroppedAttachment{
		Filename: fmt.Sprintf("p%02d.png", maxAcceptedAttachments), DetectedType: "image/png",
		Size: int64(len(replayPNG)), Reason: ticketing.DropReasonLimitExceeded,
	}
	if len(dropped) != 1 || dropped[0] != want {
		t.Fatalf("dropped = %+v, want [%+v]", dropped, want)
	}
}
