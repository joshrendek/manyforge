// Finding: SECREVIEW-M5 — accepted attachment count was unbounded.
//
// Only the per-attachment size and the dropped list were bounded. Every allowed
// part was staged, put in the jsonb metadata, inserted as a row and uploaded
// serially while the ingest transaction stayed open, so a message under the raw
// cap carrying thousands of tiny PNGs fanned out into thousands of DB rows and
// object-store requests. sniffAttachments now enforces an accepted-part count cap
// and an aggregate decoded-byte budget; parts over either are dropped.
//
// Behavioural coverage: internal/inbox/attachment_drop_test.go
// (TestSniffAttachmentsCapsAcceptedCount, TestSniffAttachmentsCapsAcceptedTotalBytes).
// This pin fails loudly if a refactor removes the caps or raises them silently.
package security_regression

import (
	"strings"
	"testing"
)

func TestPin_SECREVIEW_M5_AcceptedAttachmentCaps(t *testing.T) {
	drop := mustRead(t, "../inbox/attachment_drop.go")
	for _, want := range []string{"maxAcceptedAttachments     = 50", "maxAcceptedAttachmentBytes = 25 << 20"} {
		if !strings.Contains(drop, want) {
			t.Errorf("accepted-attachment cap %q missing or changed", want)
		}
	}

	src := mustRead(t, "../inbox/service.go")
	body := mf017Slice(t, src, "func (s *Service) sniffAttachments(", "func (s *Service) buildAttachmentMeta(")
	for _, want := range []string{
		"len(out) >= maxAcceptedAttachments",
		"acceptedBytes+size > maxAcceptedAttachmentBytes",
		"acceptedBytes += size",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("sniffAttachments must enforce %q before staging", want)
		}
	}
	// The caps must be checked before the part is staged.
	gate := strings.Index(body, "len(out) >= maxAcceptedAttachments")
	stage := strings.Index(body, "out = append(out, sniffedAttachment{")
	if gate < 0 || stage < 0 || gate > stage {
		t.Error("the accepted-attachment caps must gate staging, not run after it")
	}
}
