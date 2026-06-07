package connectors

import (
	"context"
	"net/http"
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
func (f *fakeConnector) VerifyWebhook(headers http.Header, body []byte) error { return nil }
func (f *fakeConnector) DecodeWebhook(body []byte) (WebhookEvent, error) {
	return WebhookEvent{DeliveryID: "d-1", ExternalID: f.issue.ExternalID, Kind: "issue.updated"}, nil
}
func (f *fakeConnector) CreateIssue(ctx context.Context, draft ExternalIssueDraft) (ExternalIssue, error) {
	return ExternalIssue{ExternalID: "JIRA-NEW", URL: "https://example.test/JIRA-NEW", Title: draft.Summary}, nil
}

func TestFakeConnectorSatisfiesInterface(t *testing.T) {
	ctx := context.Background()
	var c TicketingConnector = &fakeConnector{issue: ExternalIssue{ExternalID: "JIRA-1", Title: "x"}}

	iss, err := c.FetchIssue(ctx, "JIRA-1")
	if err != nil || iss.ExternalID != "JIRA-1" {
		t.Fatalf("fetch: err=%v issue=%+v", err, iss)
	}
	cm, err := c.PostComment(ctx, "JIRA-1", "hello")
	if err != nil || cm.Body != "hello" {
		t.Fatalf("post comment: err=%v comment=%+v", err, cm)
	}
	if err := c.TransitionStatus(ctx, "JIRA-1", "Done"); err != nil {
		t.Fatalf("transition: %v", err)
	}
	ids, err := c.ListUpdatedSince(ctx, time.Unix(0, 0).UTC())
	if err != nil || len(ids) != 1 || ids[0] != "JIRA-1" {
		t.Fatalf("list: err=%v ids=%v", err, ids)
	}
	if err := c.VerifyWebhook(http.Header{}, []byte("{}")); err != nil {
		t.Fatalf("verify webhook: %v", err)
	}
	ev, err := c.DecodeWebhook([]byte("{}"))
	if err != nil || ev.ExternalID != "JIRA-1" {
		t.Fatalf("decode webhook: err=%v event=%+v", err, ev)
	}
}
