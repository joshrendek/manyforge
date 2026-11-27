//go:build integration

package mailing_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/mailing"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

func newConfirmHarness(ctx context.Context, t *testing.T) (*testdb.TestDB, mailingSeed, *mailing.Service, *capturedDeliverer) {
	t.Helper()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	seed := seedMailingTenant(ctx, t, tdb)
	svc, captured := campaignService(t, ctx, tdb, seed)
	return tdb, seed, svc, captured
}

const (
	testInstanceBase = "https://hub.example.test"
	testPortalBase   = "https://portal.example.test"
)

// TestConfirmationLinkUsesPortalHostname pins that only the double-opt-in
// confirmation link moves to the business's portal hostname.
func TestConfirmationLinkUsesPortalHostname(t *testing.T) {
	cases := []struct {
		name   string
		portal func(context.Context, uuid.UUID) (string, bool)
		want   string
	}{
		{"nil resolver", nil, testInstanceBase + "/m/confirm/"},
		{"no verified domain", func(context.Context, uuid.UUID) (string, bool) { return "", false }, testInstanceBase + "/m/confirm/"},
		{"empty base reported found", func(context.Context, uuid.UUID) (string, bool) { return "", true }, testInstanceBase + "/m/confirm/"},
		{"verified domain", func(context.Context, uuid.UUID) (string, bool) { return testPortalBase + "/", true }, testPortalBase + "/m/confirm/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			tdb, seed, svc, captured := newConfirmHarness(ctx, t)
			defer tdb.Close(ctx)
			svc.PortalBaseURL = tc.portal
			list, err := svc.CreateList(ctx, seed.principalID, seed.businessID, mailing.ListInput{Name: "Updates", DoubleOptIn: true})
			if err != nil {
				t.Fatal(err)
			}
			key, err := svc.CreateListKey(ctx, seed.principalID, seed.businessID, list.ID, nil)
			if err != nil {
				t.Fatal(err)
			}
			h := mailing.NewPublicHandler(svc, nil, svc.Sealer, func(*http.Request) string { return "203.0.113.7" })
			router := chi.NewRouter()
			router.Route("/api/v1", h.PublicRoutes)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/mailing/public/"+key.PublishableKey+"/subscribe",
				strings.NewReader(`{"email":"subscriber@example.com"}`))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			if w.Code != http.StatusAccepted {
				t.Fatalf("subscribe status = %d", w.Code)
			}
			if !strings.Contains(captured.mail.BodyText, tc.want) {
				t.Fatalf("confirmation mail lacks %q: %q", tc.want, captured.mail.BodyText)
			}
			for _, other := range []string{testInstanceBase, testPortalBase} {
				if other != strings.TrimSuffix(tc.want, "/m/confirm/") && strings.Contains(captured.mail.BodyText, other+"/m/confirm/") {
					t.Fatalf("confirmation mail also links %q: %q", other, captured.mail.BodyText)
				}
			}
		})
	}
}

// TestCampaignLinksStayOnInstanceBaseWithPortal pins that unsubscribe, open
// and click links in delivered mail never move to a portal hostname, since a
// domain can be removed while the mail lives on.
func TestCampaignLinksStayOnInstanceBaseWithPortal(t *testing.T) {
	ctx := context.Background()
	tdb, seed, svc, captured := newConfirmHarness(ctx, t)
	defer tdb.Close(ctx)
	svc.PortalBaseURL = func(context.Context, uuid.UUID) (string, bool) { return testPortalBase, true }
	list, err := svc.CreateList(ctx, seed.principalID, seed.businessID, mailing.ListInput{Name: "News"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateSubscriber(ctx, seed.principalID, seed.businessID, list.ID, mailing.SubscriberInput{
		Email: "subscriber@example.com", SkipConfirmation: true, ConsentSource: "manual",
	}); err != nil {
		t.Fatal(err)
	}
	campaign, err := svc.CreateCampaign(ctx, seed.principalID, seed.businessID, mailing.CampaignInput{
		ListID: list.ID, Name: "News", Subject: "News", BodyMarkdown: "[Read](https://example.test/post)",
		TrackOpens: true, TrackClicks: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SendCampaign(ctx, seed.principalID, seed.businessID, campaign.ID, nil); err != nil {
		t.Fatal(err)
	}
	worker := &mailing.SendWorker{Service: svc, Batch: 10, Lease: 120_000_000_000}
	for i := 0; i < 3 && len(captured.mails) == 0; i++ {
		if err := worker.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if len(captured.mails) == 0 {
		t.Fatal("campaign mail was not delivered")
	}
	mail := captured.mails[0]
	body := mail.BodyHTML + "\n" + mail.BodyText + "\n" + mail.ExtraHeaders["List-Unsubscribe"]
	for _, marker := range []string{"/m/u/", "/m/o/", "/m/c/"} {
		if !strings.Contains(body, testInstanceBase+marker) {
			t.Errorf("mail lacks %s%s: %q", testInstanceBase, marker, body)
		}
	}
	if strings.Contains(body, testPortalBase) {
		t.Errorf("mail links the portal hostname: %q", body)
	}
}
