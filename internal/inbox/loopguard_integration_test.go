//go:build integration

package inbox

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/manyforge/manyforge/internal/platform/blob"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

// autoReplyRaw builds an RFC822 message flagged as a machine auto-reply
// (Auto-Submitted: auto-replied) — the RFC 3834 loop signal deriveIsAutoReply keys on.
func autoReplyRaw(addr, from, subject, messageID, inReplyTo string) RawMessage {
	msg := "From: " + from + "\r\n" +
		"To: " + addr + "\r\n" +
		"Subject: " + subject + "\r\n" +
		"Date: Mon, 02 Jan 2006 15:04:05 -0700\r\n" +
		"Message-ID: <" + messageID + ">\r\n" +
		"Auto-Submitted: auto-replied\r\n"
	if inReplyTo != "" {
		msg += "In-Reply-To: <" + inReplyTo + ">\r\n" +
			"References: <" + inReplyTo + ">\r\n"
	}
	msg += "MIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"automated reply\r\n"
	return RawMessage{
		Provider:          "webhook:test",
		EnvelopeRecipient: addr,
		EnvelopeSender:    from,
		ReceivedAt:        time.Now(),
		Raw:               []byte(msg),
	}
}

// TestLoopGuardBoundsAutoReplies pins SC-011/FR-018: a runaway auto-responder is
// bounded. With a per-requester cap of 3 auto-replies, the first three from the same
// sender are stored (one opens a ticket, the rest thread onto it) but the fourth is
// suppressed — no message, no extra ticket — and the suppression is audited.
func TestLoopGuardBoundsAutoReplies(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	ten := seedIngestTenant(ctx, t, tdb)

	store, err := blob.Open(ctx, "file://"+t.TempDir())
	if err != nil {
		t.Fatalf("open blob store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	svc := NewService(tdb.App, store, Config{
		ReplyTokenKey:           []byte("test-reply-token-key-0123456789ab"),
		AttachmentMaxBytes:      25 << 20,
		InboundSystemDomain:     systemDomain,
		LoopGuardMaxAutoReplies: 3,
	}, slog.New(slog.NewTextHandler(nopWriter{}, nil)))

	const sender = "autobot@example.com"

	// First auto-reply opens a ticket; #2 and #3 thread onto it; all three stored.
	first, err := svc.Ingest(ctx, autoReplyRaw(ten.address, sender, "loop", "loop-1@example.com", ""))
	if err != nil {
		t.Fatalf("ingest #1: %v", err)
	}
	if first.Suppressed {
		t.Fatal("first auto-reply should not be suppressed")
	}
	for i := 2; i <= 3; i++ {
		mid := fmt.Sprintf("loop-%d@example.com", i)
		res, ierr := svc.Ingest(ctx, autoReplyRaw(ten.address, sender, "loop", mid, "loop-1@example.com"))
		if ierr != nil {
			t.Fatalf("ingest #%d: %v", i, ierr)
		}
		if res.Suppressed {
			t.Fatalf("auto-reply #%d should not be suppressed (cap is 3)", i)
		}
	}

	// The fourth exceeds the cap → suppressed: no message persisted, audited.
	fourth, err := svc.Ingest(ctx, autoReplyRaw(ten.address, sender, "loop", "loop-4@example.com", "loop-1@example.com"))
	if err != nil {
		t.Fatalf("ingest #4: %v", err)
	}
	if !fourth.Suppressed {
		t.Errorf("fourth auto-reply: Suppressed=false, want true (per-requester cap exceeded)")
	}

	if n := countSuper(ctx, t, tdb.Super,
		"SELECT count(*) FROM ticket_message WHERE business_id=$1 AND is_auto_reply", ten.master); n != 3 {
		t.Errorf("stored auto-reply messages = %d, want 3 (the 4th must be suppressed)", n)
	}
	if n := countSuper(ctx, t, tdb.Super,
		"SELECT count(*) FROM audit_entry WHERE business_id=$1 AND action='ticket.loop_suppressed'", ten.master); n != 1 {
		t.Errorf("loop_suppressed audit entries = %d, want 1", n)
	}
}
