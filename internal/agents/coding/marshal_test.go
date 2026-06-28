package coding

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/connectors"
)

// TestCodeReviewMarshalSnakeCase asserts that CodeReview serialises to the
// snake_case keys mandated by the OpenAPI contract and the rest of the API.
// It is intentionally written BEFORE json tags are added so it fails first
// (TDD red phase), then passes once tags are added (green phase).
func TestCodeReviewMarshalSnakeCase(t *testing.T) {
	ln := 42
	postedAt := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	cr := CodeReview{
		ID:     uuid.MustParse("00000000-0000-0000-0000-000000000001"),
		Status: "succeeded",
		Summary: "Looks good",
		ReviewURL: "https://github.com/owner/repo/pull/1#pullrequestreview-99",
		PRNumber:      7,
		Findings:      []connectors.Finding{{File: "main.go", Line: &ln, Severity: "warning", Title: "unused var", Detail: "x declared but not used"}},
		FindingsCount: 1,
		CreatedAt:     time.Date(2026, 6, 25, 10, 0, 0, 0, time.UTC),
		PostedAt:      &postedAt,
	}

	data, err := json.Marshal(cr)
	if err != nil {
		t.Fatalf("json.Marshal(CodeReview): %v", err)
	}
	s := string(data)

	wantKeys := []string{"id", "status", "summary", "review_url", "pr_number", "findings", "findings_count", "created_at", "posted_at"}
	for _, k := range wantKeys {
		if !strings.Contains(s, `"`+k+`"`) {
			t.Errorf("missing snake_case key %q in JSON output: %s", k, s)
		}
	}

	// PascalCase keys must NOT appear.
	badKeys := []string{"ID", "Status", "Summary", "ReviewURL", "PRNumber", "Findings", "FindingsCount", "CreatedAt", "PostedAt"}
	for _, k := range badKeys {
		if strings.Contains(s, `"`+k+`"`) {
			t.Errorf("PascalCase key %q must not appear in JSON output: %s", k, s)
		}
	}
}

// TestCodeReviewPostedAtNilOmitted verifies that a nil PostedAt serialises as
// JSON null (per the OpenAPI nullable: true schema) rather than being omitted.
// If the openapi schema ever changes to optional (omitempty), update this test.
func TestCodeReviewPostedAtNilIncluded(t *testing.T) {
	cr := CodeReview{
		ID:     uuid.MustParse("00000000-0000-0000-0000-000000000002"),
		Status: "pending",
	}

	data, err := json.Marshal(cr)
	if err != nil {
		t.Fatalf("json.Marshal(CodeReview): %v", err)
	}
	s := string(data)

	// posted_at is nullable in the OpenAPI schema; it should serialise as null
	// (pointer type, no omitempty) so the field is present with a null value.
	if !strings.Contains(s, `"posted_at":null`) {
		t.Errorf("expected posted_at:null for nil PostedAt, got: %s", s)
	}
}
