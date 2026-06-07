package connectors

import (
	"context"
	"testing"
	"time"
)

// fakeConnector is a canned in-memory TicketingConnector: it proves the interface is
// implementable and lets the Registry test run without a real external API.
type fakeConnector struct {
	issue ExternalIssue
}

var _ TicketingConnector = (*fakeConnector)(nil)

func (f *fakeConnector) FetchIssue(ctx context.Context, externalID string) (ExternalIssue, error) {
	return f.issue, nil
}
func (f *fakeConnector) PostComment(ctx context.Context, externalID, body string) (ExternalComment, error) {
	return ExternalComment{ExternalID: "c-1", Body: body, CreatedAt: time.Unix(0, 0).UTC()}, nil
}
func (f *fakeConnector) TransitionStatus(ctx context.Context, externalID, status string) error {
	return nil
}
func (f *fakeConnector) ListUpdatedSince(ctx context.Context, since time.Time) ([]string, error) {
	return []string{f.issue.ExternalID}, nil
}
func (f *fakeConnector) VerifyWebhook(headers map[string]string, body []byte) error { return nil }
func (f *fakeConnector) DecodeWebhook(body []byte) (WebhookEvent, error) {
	return WebhookEvent{DeliveryID: "d-1", ExternalID: f.issue.ExternalID, Kind: "issue.updated"}, nil
}

func TestFakeConnectorSatisfiesInterface(t *testing.T) {
	var c TicketingConnector = &fakeConnector{issue: ExternalIssue{ExternalID: "JIRA-1", Title: "x"}}
	iss, err := c.FetchIssue(context.Background(), "JIRA-1")
	if err != nil || iss.ExternalID != "JIRA-1" {
		t.Fatalf("fake fetch: err=%v issue=%+v", err, iss)
	}
}
