package agents

import (
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

type runKeyset struct {
	ts time.Time
	id uuid.UUID
}

const runCursorSep = "|"

func encodeRunCursor(k runKeyset) string {
	raw := "run" + runCursorSep + k.ts.UTC().Format(time.RFC3339Nano) + runCursorSep + k.id.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeRunCursor(token string) (runKeyset, error) {
	bad := func() (runKeyset, error) {
		return runKeyset{}, fmt.Errorf("invalid cursor: %w", errs.ErrValidation)
	}
	dec, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return bad()
	}
	parts := strings.SplitN(string(dec), runCursorSep, 3)
	if len(parts) != 3 || parts[0] != "run" {
		return bad()
	}
	ts, err := time.Parse(time.RFC3339Nano, parts[1])
	if err != nil {
		return bad()
	}
	id, err := uuid.Parse(parts[2])
	if err != nil {
		return bad()
	}
	return runKeyset{ts: ts, id: id}, nil
}
