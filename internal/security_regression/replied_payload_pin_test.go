// No build tag: this source-level pin runs in both `make test` and `make sec-test`
// with no infrastructure (manyforge-bu7). It guards the #1 producer→consumer seam
// in the support send path: Reply (internal/ticketing/service.go) enqueues a
// map[string]any onto the outbox, and SendSubscriber (internal/platform/notify/
// sender_subscriber.go) decodes it into `repliedPayload` BY JSON TAG. The keys MUST
// match exactly. A rename on EITHER side decodes the renamed field to its zero value
// -> get_send_context finds no message row -> the reply is dropped as a terminal
// no-op (warn only). No behavioral test fails today (reply_integration_test only
// asserts an outbox row exists; send_integration_test builds its own payload map),
// so this pin is the loud-on-drift backstop the contract relies on.

package security_regression

import (
	"strings"
	"testing"
)

// TestRepliedPayloadSeamPinned asserts every key SendSubscriber.repliedPayload
// decodes is emitted by Reply's events.Enqueue map under the identical name, and
// that both sides still dispatch on the same topic constant.
func TestRepliedPayloadSeamPinned(t *testing.T) {
	producer := mustRead(t, "../ticketing/service.go")                 // Reply: events.Enqueue(map[string]any{...})
	consumer := mustRead(t, "../platform/notify/sender_subscriber.go") // SendSubscriber: repliedPayload json tags

	// The shared decode contract. `ticket_id` is producer-only by design (Reply emits
	// it; repliedPayload intentionally does not decode it) so it is NOT pinned here —
	// adding it to repliedPayload later is a deliberate change, not silent drift.
	keys := []string{
		"message_row_id",
		"recipient",
		"subject",
		"rfc_message_id",
		"in_reply_to",
		"references",
		"reply_token",
		"business_id",
	}
	for _, k := range keys {
		if !strings.Contains(producer, `"`+k+`":`) {
			t.Errorf("producer drift: Reply no longer enqueues %q (service.go) — SendSubscriber would decode it to a zero value and the reply would be dropped silently", k)
		}
		if !strings.Contains(consumer, `json:"`+k+`"`) {
			t.Errorf("consumer drift: repliedPayload no longer decodes json:%q (sender_subscriber.go) — Reply still emits it but the send path would ignore it", k)
		}
	}

	// Both sides key off the same topic constant, and the reply/send integration
	// tests assert the literal 'ticket.replied' in SQL — pin the constant's value so
	// the dispatch key and those assertions cannot diverge.
	bus := mustRead(t, "../platform/events/bus.go")
	if !strings.Contains(bus, `TopicTicketReplied = "ticket.replied"`) {
		t.Error(`topic drift: events.TopicTicketReplied is no longer "ticket.replied" — the outbox dispatch key and the *_integration_test SQL assertions would diverge`)
	}
}
