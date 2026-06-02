package main

// conversation is one ticket: the first message opens it; each subsequent message
// threads onto it via In-Reply-To. Message-IDs are STABLE so re-running the seed is
// an idempotent no-op (the DEFINER dedupes on (tenant_root_id, message_id)).
type seedMsg struct {
	From      string // RFC5322 From (display + addr)
	Subject   string
	MessageID string // without angle brackets; must be globally stable per seed
	InReplyTo string // "" for the opening message
	Body      string
}

type conversation struct {
	Key  string // short slug used to build stable message-ids per business
	Msgs []seedMsg
}

// conversationsFor returns the demo conversations for a business slug. Content spans
// a realistic mix so the list shows variety (open/threaded, different requesters).
func conversationsFor(bizSlug string) []conversation {
	return []conversation{
		{
			Key: "pw",
			Msgs: []seedMsg{
				{
					From:      "Jane Customer <jane@example.com>",
					Subject:   "Cannot reset my password",
					MessageID: "seed-" + bizSlug + "-pw-1@demo.manyforge.test",
					InReplyTo: "",
					Body:      "Hi, the reset link in your email returns 'token expired' every time. Help?",
				},
				{
					From:      "Jane Customer <jane@example.com>",
					Subject:   "Re: Cannot reset my password",
					MessageID: "seed-" + bizSlug + "-pw-2@demo.manyforge.test",
					InReplyTo: "seed-" + bizSlug + "-pw-1@demo.manyforge.test",
					Body:      "Still stuck — I tried three times over the last hour.",
				},
			},
		},
		{
			Key: "billing",
			Msgs: []seedMsg{
				{
					From:      "Marcus Reed <marcus@globex.test>",
					Subject:   "Double charged this month",
					MessageID: "seed-" + bizSlug + "-billing-1@demo.manyforge.test",
					InReplyTo: "",
					Body:      "I see two identical charges on my card for the same invoice. Please refund one.",
				},
			},
		},
		{
			Key: "feature",
			Msgs: []seedMsg{
				{
					From:      "Priya Nair <priya@initech.test>",
					Subject:   "Feature request: CSV export",
					MessageID: "seed-" + bizSlug + "-feature-1@demo.manyforge.test",
					InReplyTo: "",
					Body:      "Would love a way to export my data as CSV from the dashboard.",
				},
			},
		},
	}
}
