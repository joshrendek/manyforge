// Package ticketing is the support-desk ticket domain (spec 002): tickets,
// messages, requesters, tags, replies, internal notes, triage, and the custom
// sending-identity (email-domain) lifecycle.
//
// All reads and mutations are dual-enforced (self-deriving RLS + an app-level
// business/tenant predicate), audited in the same transaction as the mutation,
// and return an identical not-found shape for unknown and cross-tenant
// resources (no allowed-vs-exists oracle), inheriting the spec-001 foundation.
package ticketing
