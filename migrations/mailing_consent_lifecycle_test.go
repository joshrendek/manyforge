package migrations

import (
	"strings"
	"testing"
)

func TestMailingConsentLifecycleMigrationDoesNotScanOrRewriteLegacyTrackingEvents(t *testing.T) {
	raw, err := FS.ReadFile("0133_mailing_consent_lifecycle.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := strings.ToUpper(string(raw))
	for _, forbidden := range []string{
		"ALTER TABLE MAILING_TRACKING_EVENT",
		"UPDATE MAILING_TRACKING_EVENT",
		"DELETE FROM MAILING_TRACKING_EVENT",
		"CREATE UNIQUE INDEX MAILING_TRACKING_EVENT",
		"FROM MAILING_TRACKING_EVENT",
		"JOIN MAILING_TRACKING_EVENT",
	} {
		if strings.Contains(sql, forbidden) {
			t.Fatalf("0133 performs deployment-time work over legacy tracking events: %q", forbidden)
		}
	}
}
