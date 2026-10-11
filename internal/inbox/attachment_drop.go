package inbox

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/ticketing"
)

// maxDroppedAttachments matches the ticket_message.dropped_attachments CHECK.
const maxDroppedAttachments = 50

// maxDroppedFilenameRunes bounds an attacker-controlled filename kept for display.
const maxDroppedFilenameRunes = 255

// sanitizeAttachmentFilename drops invalid UTF-8 and control characters (Postgres
// jsonb rejects \u0000, which would otherwise fail the whole ingest) and bounds
// the length.
func sanitizeAttachmentFilename(name string) string {
	var b strings.Builder
	runes := 0
	for len(name) > 0 && runes < maxDroppedFilenameRunes {
		r, size := utf8.DecodeRuneInString(name)
		name = name[size:]
		if r == utf8.RuneError && size <= 1 {
			continue
		}
		if unicode.IsControl(r) {
			continue
		}
		b.WriteRune(r)
		runes++
	}
	return b.String()
}

// recordDroppedAttachments stores the dropped list on the just-inserted inbound
// message through the SECURITY DEFINER (the ingest tx is principal-less, so a
// plain UPDATE would match zero rows under RLS). A false return means the message
// row was not updated; that is an error so a drop is never silently lost.
func recordDroppedAttachments(ctx context.Context, tx pgx.Tx, messageID, tenantRootID uuid.UUID, dropped []ticketing.DroppedAttachment) error {
	raw, err := json.Marshal(dropped)
	if err != nil {
		return fmt.Errorf("inbox: marshal dropped attachments: %w", err)
	}
	var updated bool
	if err := tx.QueryRow(ctx, "SELECT set_message_dropped_attachments($1, $2, $3)",
		messageID, tenantRootID, raw).Scan(&updated); err != nil {
		return fmt.Errorf("inbox: record dropped attachments: %w", err)
	}
	if !updated {
		return fmt.Errorf("inbox: record dropped attachments: message %s not updated", messageID)
	}
	return nil
}
