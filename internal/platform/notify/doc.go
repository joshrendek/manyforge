// Package notify is the thin first cut of shared layer SL-D (spec 002):
// in-app notifications plus templated, threaded, domain-authenticated email
// that extends the spec-001 Mailer and reuses its email_suppression for bounce
// suppression. Delivery is driven by the SL-C outbox worker.
package notify
