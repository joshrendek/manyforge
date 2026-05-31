// Package inbox is the inbound-ingestion domain for the support desk (spec 002).
//
// It defines a pluggable InboundSource (a provider webhook adapter and an
// in-process SMTP receiver), resolves each message to exactly one business by
// recipient address (no existence oracle on unknown recipients), threads it
// onto a ticket, and persists it through the audited, business-scoped
// SECURITY DEFINER ingestion path. Ticketing logic never sees a provider shape.
package inbox
