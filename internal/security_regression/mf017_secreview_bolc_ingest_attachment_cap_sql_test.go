// Finding: manyforge-bolc — SECREVIEW-M5 follow-up: the accepted-attachment caps
// lived only in Go (internal/inbox sniffAttachments). ingest_inbound_message
// inserted every element of p_attachments, so any caller that skipped or regressed
// the Go caps could fan one message out into unbounded attachment rows. Migration
// 0143 makes the function reject more than 50 attachments or more than 25 MiB of
// declared sizes with SQLSTATE IN413, before any write.
//
// Behavioural test: internal/inbox/ingest_attachment_cap_integration_test.go
// (TestIngestSQLAttachmentCaps). This pin keeps the SQL literals equal to the Go
// constants, so raising one without the other fails here.
package security_regression

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// goAttachmentCaps parses maxAcceptedAttachments / maxAcceptedAttachmentBytes from
// internal/inbox/attachment_drop.go. Each is an integer literal or "a << b".
func goAttachmentCaps(t *testing.T) (count, bytes int64) {
	t.Helper()
	src := mustRead(t, "../inbox/attachment_drop.go")
	parseConst := func(name string) int64 {
		m := regexp.MustCompile(`(?m)^\s*` + name + `\s*=\s*(\d+)(?:\s*<<\s*(\d+))?\s*$`).FindStringSubmatch(src)
		if m == nil {
			t.Fatalf("attachment_drop.go: cannot parse const %s (expected an int literal or a << b)", name)
		}
		v, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		if m[2] != "" {
			shift, err := strconv.ParseInt(m[2], 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			v <<= shift
		}
		return v
	}
	return parseConst("maxAcceptedAttachments"), parseConst("maxAcceptedAttachmentBytes")
}

func TestPin_BOLC_IngestAttachmentCapsInSQL(t *testing.T) {
	count, bytes := goAttachmentCaps(t)
	fn, file := secreviewLatestFunction(t, "ingest_inbound_message")
	countCheck := "IF jsonb_array_length(p_attachments) > " + strconv.FormatInt(count, 10) + " THEN"
	bytesCheck := "IF v_attachment_bytes > " + strconv.FormatInt(bytes, 10) + " THEN"
	for _, want := range []string{
		countCheck,
		"SELECT COALESCE(sum((a->>'size')::bigint), 0) INTO v_attachment_bytes",
		bytesCheck,
		"USING ERRCODE = 'IN413'",
	} {
		if !strings.Contains(fn, want) {
			t.Errorf("%s ingest_inbound_message must contain %q (matching the Go caps)", file, want)
		}
	}
	if got := strings.Count(fn, "USING ERRCODE = 'IN413'"); got != 2 {
		t.Errorf("%s: %d IN413 raises, want 2 (count and bytes)", file, got)
	}

	// The caps must be enforced before anything is written or locked.
	ci := strings.Index(fn, countCheck)
	bi := strings.Index(fn, bytesCheck)
	for _, later := range []string{"pg_advisory_xact_lock", "INSERT INTO requester", "INSERT INTO ticket (", "INSERT INTO ticket_message", "INSERT INTO attachment"} {
		if i := strings.Index(fn, later); i < 0 || ci < 0 || bi < 0 || i < ci || i < bi {
			t.Errorf("%s: %q must come after the attachment caps", file, later)
		}
	}

	// Privilege shape unchanged: still a pinned-search_path DEFINER.
	for _, want := range []string{"SECURITY DEFINER", "SET search_path = public"} {
		if !strings.Contains(fn, want) {
			t.Errorf("%s ingest_inbound_message lost %q", file, want)
		}
	}
}
