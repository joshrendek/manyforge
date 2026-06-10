//go:build integration

package connectors

// TestBidirectionalRoundTrip is the US4 capstone: it proves the whole Jira loop end-to-end
// against ONE httptest Jira stub reached through the netsafe SSRF-safe HTTP client (spec §10
// demo). No new product code — it composes the existing US3 inbound + US4 outbound helpers
// into one genuine round-trip:
//
//	(1) an inbound webhook is delivered to the public handler (real HMAC verify) -> the handler
//	    writes a connector.inbound.sync outbox event -> the InboundSyncSubscriber drains it,
//	    FetchIssue-es the external issue, and upserts the native ticket (connector-linked);
//	(2) an operator reply on that now-linked ticket goes through the GENUINE ticketing.Service
//	    .Reply producer (the Task 3 hook), which enqueues a pending 'comment' outbound op in the
//	    SAME source tx (NOT a raw queue insert);
//	(3) the OutboundDispatcher claims the op, PostComment-s it to the stub through the SSRF
//	    client, and writes the returned comment external_id back onto the native message.
//
// Non-vacuity guards: the stub increments commentPosts on a REAL POST (asserted ==1), and the
// operator message's external_id must flip from NULL to non-null (asserted via
// operatorMessageHasExternalID). The create-issue direction is left to T5's dedicated test
// (TestOutboundDispatcherCreatesIssue) to keep this env focused on the required inbound->reply
// ->comment core.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBidirectionalRoundTrip(t *testing.T) {
	ctx, tdb, seed := startConn(t)

	var commentPosts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comment"):
			commentPosts++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"jc-rt","author":{"displayName":"ops"},"created":"2026-06-07T00:00:00.000+0000"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	// connector + inbound subscriber + outbound dispatcher + ticketing producer + webhook handler,
	// all behind the one SSRF-stubbed Jira at srv.URL (127.0.0.1, allow_private_base_url=true).
	env := seedFullConnectorEnv(t, ctx, tdb, seed, srv.URL)

	// (1) Inbound: deliver a signed webhook -> the subscriber upserts the native ticket.
	env.deliverWebhook(t, ctx, "JIRA-RT", 1000)
	env.runInboundOnce(t, ctx)
	ticketID := ticketByExternal(t, ctx, tdb, env.ConnectorID, "JIRA-RT")

	// (2) Outbound: an operator reply on the now-linked ticket enqueues a comment op through the
	// real producer path (ticketing.Service.Reply -> EnqueueOutboundComment in the same tx).
	env.replyAsOperator(t, ctx, ticketID, "we pushed a fix")

	// (3) Dispatch -> the stub receives the comment POST; the message external_id is written back.
	if err := env.Dispatcher.dispatchOnce(ctx); err != nil {
		t.Fatalf("dispatchOnce: %v", err)
	}
	if commentPosts != 1 {
		t.Fatalf("comment posts = %d, want 1 (the reply must have been posted to Jira via a REAL POST)", commentPosts)
	}
	if !operatorMessageHasExternalID(t, ctx, tdb, ticketID) {
		t.Fatalf("operator reply was not linked back to a Jira comment id (external_id still NULL)")
	}
}
