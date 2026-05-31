package security_regression

// Spec 002 (Native Support Desk) security-regression finding IDs.
//
// Each support pin/behavioral test file carries one of these IDs in its header
// comment so a refactor that silently drops an invariant is traceable in CI.
// The pins (source-level `strings.Contains` / reflection checks) run under the
// untagged `make test`; the behavioral matrices run under `make sec-test`
// (`//go:build integration`).
const (
	// FindingSupportIsolation — RLS + app-predicate dual isolation and the
	// identical-404 no-oracle boundary across the seven new tenant tables (US5).
	FindingSupportIsolation = "MF-002-ISOLATION"

	// FindingIngestionScope — the SECURITY DEFINER ingestion function cannot
	// widen beyond the single resolved business (US1, FR-017).
	FindingIngestionScope = "MF-002-INGEST-SCOPE"

	// FindingThreadingIdempotency — replayed Message-ID never double-creates and
	// no message ever attaches to the wrong ticket; forged reply tokens are
	// rejected by constant-time HMAC (US1, FR-004/FR-005).
	FindingThreadingIdempotency = "MF-002-THREAD-IDEMPOTENCY"

	// FindingMIMESniff — attachment content type is sniffed and allowlisted; the
	// declared Content-Type is never trusted (US1, FR-007).
	FindingMIMESniff = "MF-002-MIME-SNIFF"

	// FindingWebhookSig — provider webhook HMAC is verified in constant time
	// (US1, FR-020).
	FindingWebhookSig = "MF-002-WEBHOOK-SIG"
)
