//go:build integration

// Regression for the orphaned-blob leak fixed in T027: on a replay of a
// message-with-attachments, ingest_inbound_message returns out_duplicate and
// inserts NO attachment rows — so the ingest path must write ZERO new blobs.
// Before the fix, blobs were Put before the DEFINER call and leaked on every
// (attacker-replayable) replay. This asserts both the DB attachment row count and
// the on-disk fileblob object count stay at exactly one across a replay.

package inbox

import (
	"context"
	"encoding/base64"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/manyforge/manyforge/internal/platform/blob"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

// minimal valid PNG (signature + IHDR start); http.DetectContentType → image/png,
// which is in blob.Sniff's allowlist, so it is stored.
var replayPNG = []byte{
	0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a,
	0x00, 0x00, 0x00, 0x0d, 'I', 'H', 'D', 'R',
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4, 0x89,
}

func pngAttachmentMessage(to, from, subject, messageID string, attachment []byte) []byte {
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

// countBlobFiles counts the regular files under dir (the fileblob backend writes
// one object per Put; gocloud may also write *.attrs sidecars, which we ignore so
// the count reflects stored objects, not metadata).
func countBlobFiles(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(d.Name()) == ".attrs" {
			return nil
		}
		n++
		return nil
	})
	if err != nil {
		t.Fatalf("walk blob dir: %v", err)
	}
	return n
}

// TestIngestReplayDoesNotLeakBlobs (T027 fix): replaying a message that carries an
// attachment writes the blob exactly once. The replay reports Duplicate, persists
// no second attachment row, and — the security property — leaves the blob count at
// one (no orphaned object).
func TestIngestReplayDoesNotLeakBlobs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	ten := seedIngestTenant(ctx, t, tdb)

	// An inspectable blob dir (not the helper's hidden one) so we can count objects.
	blobDir := t.TempDir()
	store, err := blob.Open(ctx, "file://"+blobDir)
	if err != nil {
		t.Fatalf("open blob store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := Config{
		ReplyTokenKey:       []byte("test-reply-token-key-0123456789ab"),
		AttachmentMaxBytes:  25 << 20,
		InboundSystemDomain: systemDomain,
	}
	svc := NewService(tdb.App, store, cfg, slog.New(slog.NewTextHandler(nopWriter{}, nil)))

	raw := RawMessage{
		Provider:          "webhook:test",
		EnvelopeRecipient: ten.address,
		EnvelopeSender:    "ada@example.com",
		ReceivedAt:        time.Now(),
		Raw:               pngAttachmentMessage(ten.address, "Ada <ada@example.com>", "with attachment", "attach-replay@example.com", replayPNG),
	}

	res1, err := svc.Ingest(ctx, raw)
	if err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	if res1.Duplicate {
		t.Fatalf("first ingest reported Duplicate, want a fresh insert")
	}
	if n := countSuper(ctx, t, tdb.Super, "SELECT count(*) FROM attachment WHERE business_id=$1", ten.master); n != 1 {
		t.Fatalf("attachment row count after first ingest = %d, want 1", n)
	}
	if n := countBlobFiles(t, blobDir); n != 1 {
		t.Fatalf("blob object count after first ingest = %d, want 1", n)
	}

	// Replay the SAME Message-ID: idempotent no-op. No new attachment row, and —
	// the regression — no second blob written (the pre-fix path leaked one here).
	res2, err := svc.Ingest(ctx, raw)
	if err != nil {
		t.Fatalf("replay ingest: %v", err)
	}
	if !res2.Duplicate {
		t.Errorf("replay Duplicate = false, want true")
	}
	if n := countSuper(ctx, t, tdb.Super, "SELECT count(*) FROM attachment WHERE business_id=$1", ten.master); n != 1 {
		t.Errorf("attachment row count after replay = %d, want 1 (no duplicate row)", n)
	}
	if n := countBlobFiles(t, blobDir); n != 1 {
		t.Errorf("blob object count after replay = %d, want 1 — replay must not leak an orphaned blob", n)
	}
}
