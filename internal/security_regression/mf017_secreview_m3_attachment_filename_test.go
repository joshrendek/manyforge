// Finding: SECREVIEW-M3 — valid attachment filenames were not sanitized.
//
// An allowlisted attachment kept its raw, attacker-controlled filename. An RFC
// 2231 filename*=UTF-8''bad%00name.png decodes to a NUL, which Postgres jsonb
// rejects, so ingest failed, the Resend webhook answered 503, and Resend retried
// the same poison message forever. Every staged (stored) filename must go through
// the same sanitizer + length bound the dropped list already used.
//
// Behavioural coverage: internal/inbox/attachment_drop_test.go
// (TestSniffAttachmentsSanitizesAllowedFilename) and the integration test
// TestIngestStoredAttachmentFilenameWithNULStillIngests next to it. This pin
// fails loudly if a refactor drops the sanitizer from the staging path.
package security_regression

import (
	"strings"
	"testing"
)

func TestPin_SECREVIEW_M3_StagedFilenameSanitized(t *testing.T) {
	src := mustRead(t, "../inbox/service.go")
	body := mf017Slice(t, src, "func (s *Service) sniffAttachments(", "func (s *Service) buildAttachmentMeta(")
	staged := mf017Slice(t, body, "sniffedAttachment{", "})")
	if !strings.Contains(staged, "filename:    sanitizeAttachmentFilename(p.FileName)") {
		t.Error("staged attachments must store sanitizeAttachmentFilename(p.FileName), never the raw filename")
	}
	if strings.Contains(body, "filename:    p.FileName") {
		t.Error("a raw p.FileName must never be staged for storage")
	}

	drop := mustRead(t, "../inbox/attachment_drop.go")
	fn := mf017Slice(t, drop, "func sanitizeAttachmentFilename(", "\n}\n")
	for _, want := range []string{"runes < maxAttachmentFilenameRunes", "unicode.IsControl(r)", "utf8.RuneError"} {
		if !strings.Contains(fn, want) {
			t.Errorf("sanitizeAttachmentFilename lost %q (length bound / control strip / invalid UTF-8 drop)", want)
		}
	}
}
