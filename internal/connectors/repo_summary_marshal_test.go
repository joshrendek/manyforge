package connectors

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestRepoConnectorSummaryMarshalSnakeCase asserts that RepoConnectorSummary
// serialises to the snake_case keys mandated by the OpenAPI contract.
// Written BEFORE json tags are added (TDD red phase).
func TestRepoConnectorSummaryMarshalSnakeCase(t *testing.T) {
	rcs := RepoConnectorSummary{
		ID:                  "00000000-0000-0000-0000-000000000003",
		Type:                "github",
		DisplayName:         "Acme GitHub",
		BaseURL:             "https://api.github.com",
		Repo:                "acme/backend",
		AllowPrivateBaseURL: false,
		Status:              "enabled",
		CreatedAt:           time.Date(2026, 6, 25, 10, 0, 0, 0, time.UTC),
	}

	data, err := json.Marshal(rcs)
	if err != nil {
		t.Fatalf("json.Marshal(RepoConnectorSummary): %v", err)
	}
	s := string(data)

	wantKeys := []string{"id", "type", "display_name", "base_url", "repo", "allow_private_base_url", "status", "created_at"}
	for _, k := range wantKeys {
		if !strings.Contains(s, `"`+k+`"`) {
			t.Errorf("missing snake_case key %q in JSON output: %s", k, s)
		}
	}

	// PascalCase keys must NOT appear.
	badKeys := []string{"ID", "Type", "DisplayName", "BaseURL", "Repo", "AllowPrivateBaseURL", "Status", "CreatedAt"}
	for _, k := range badKeys {
		if strings.Contains(s, `"`+k+`"`) {
			t.Errorf("PascalCase key %q must not appear in JSON output: %s", k, s)
		}
	}
}
